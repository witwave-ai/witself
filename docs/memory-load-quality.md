# Narrative Memory Load And Quality Harnesses

Status: five executable PostgreSQL slices. This runbook defines the original
opt-in lexical-memory baseline, the bounded curation load/lifecycle slice, the
lexical plus client-vector/hybrid recall load/quality slice, and the
whole-account archive round-trip/retrieval-projection slice, plus the
concurrent-agent and tenant-isolation slice for production-readiness issue
[#46](https://github.com/witwave-ai/witself/issues/46). They provide useful,
reproducible evidence, but individually or together, they do not close that
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

No retained result contains:

- a DSN, hostname, port, database name, or database user;
- an account, realm, agent, or memory id;
- a query, memory value, tag set, content hash, or sensitive marker;
- a vector profile id, vector hash, query vector, or stored vector components;
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

## Recall Load And Quality Slice

`TestNarrativeMemoryRecallLoadPostgres` is the third executable slice. It runs
against one PostgreSQL endpoint in a fresh, disposable schema, applies the
complete migration set, creates only synthetic tenants, agents, memories,
vector profiles, and vectors, and drops the complete schema during cleanup.
For vector storage and recall, the harness observes only the public store
surface: `CreateMemoryVectorProfile`, `ListMemoryVectorProfiles`,
`PutMemoryVector`, and `RecallMemories`. Production store code and recall
behavior are not replaced or weakened for the test.

Vectors are always supplied by the client. The backend never embeds memory or
query text, and this harness calls no AI, model, embedding service, runtime
client, MCP server, secret store, or sealed-plane operation. Synthetic vectors
are generated deterministically by expanding `SHA-256(seed:index)` to the
configured profile dimension and L2-normalizing the components. The harness
creates no agent token. Quality fixtures deliberately make a labeled memory's
vector near its query vector and distractor vectors far from it, so the expected
ranks follow from the fixture rather than chance or an embedding model.

The five named workload deadlines are `cardinality ladder`, `vector coverage`,
`hybrid relevance quality`, `vector safety`, and `pagination ordering`. Each is
two minutes inside the nine-minute overall driver context. The Make target has
a separate 12-minute `go test` timeout. A signed 64-bit seed makes every
generated corpus token and vector deterministic; store-assigned ids and wall
clock time are never random seeds.

### What The Recall Harness Proves

The bounded workload exercises and measures:

1. **Cardinality ladder.** Separate tenant fixtures contain each configured
   memory count. Concurrent workers issue the declared number of lexical-only
   recalls at every cardinality and assert that every call reports lexical mode
   and completes normally. Per-cardinality latency and throughput remain
   separate rather than being averaged across fixture sizes.
2. **Vector coverage.** Immutable client vector profiles are listed back and a
   deterministic fraction of the smallest tenant's eligible memories receives
   a vector for each configured coverage case: exactly
   `floor(memory_count * coverage_percent / 100)`. The defaults exercise both
   complete and partial coverage. Each case records attachment and hybrid-recall
   measurements, requested and reported coverage, vector candidate/match
   counts, degradation, candidate-budget metadata, whether recall metadata
   stayed stable, and whether every created immutable profile listed back
   unchanged.
3. **Hybrid relevance quality.** Three labeled cases prove that a vector-near
   memory with no lexical signal is found, a lexically matching memory without
   a vector is found, and a memory carrying both signals ranks above
   single-signal distractors. Each case records only its safe label, observed
   and maximum acceptable rank, and whether the expected lexical, similarity,
   and `vector_used` score components were observed. Version 1 pins both the
   observed and maximum acceptable rank to `1` for all three cases.
4. **Vector redaction and isolation.** Broad recall still redacts sensitive
   content, explicit exact-owner recall can reveal the synthetic sensitive
   fixture, and same-account cross-agent plus cross-account reads remain
   isolated when a client query vector and profile are supplied.
5. **Pagination and limit behavior.** At the largest cardinality, two identical
   traversals page to the configured result budget. The harness checks every
   page limit, absence of duplicate ids, the reported candidate budget and
   truncation state, and identical ranked-id order across those repeated
   queries within the same run.

Recall returns the scoring components that the assertions inspect. Lexical
mode currently uses
`0.60*lexical + 0.25*salience + 0.15*recency`; hybrid mode uses
`0.50*similarity + 0.30*lexical + 0.12*salience + 0.08*recency` and reports
whether a compatible vector contributed to each hit.

### Run The Recall Slice

Keep the DSN in the trusted parent environment and invoke the dedicated target:

```sh
make db-up
export WITSELF_TEST_DATABASE_URL='postgres://witself:witself@localhost:5432/witself?sslmode=disable'
make test-memory-recall-load
```

Unless overridden, the result path is
`/tmp/witself-memory-recall-load-<pid>.json`. It is process-scoped so concurrent
runs cannot atomically rename over one another. Every workload control is
bounded:

| Setting | Default | Allowed range |
|---|---:|---:|
| Seed | `20260831` | signed 64-bit integer |
| Tenant memory cardinalities | `100,500,2000` | 2-5 strictly increasing values, each `10..10000`; smallest at most `256`, largest greater than `256` |
| Query iterations per cardinality/coverage case | `10` | `1..1000`, and at least the configured concurrency |
| Concurrent lexical workers | `4` | `2..64` |
| Vector profile dimensions | `32` | `2..4096` |
| Vector coverage percentages | `100,50` | 2-4 strictly decreasing values; first `100`, remainder `1..99`; smallest-cardinality count at the lowest coverage must be at least one |
| Pagination limit | `64` | `1..100` |
| Pagination result budget | `256` | `2..256`, greater than the page limit and no greater than the largest cardinality |

Override only bounded workload and safe evidence metadata:

```sh
make test-memory-recall-load \
  MEMORY_RECALL_LOAD_RESULTS=/trusted-artifacts/memory-recall-load.json \
  MEMORY_RECALL_LOAD_SEED=20260831 \
  MEMORY_RECALL_LOAD_CARDINALITIES=100,500,2000 \
  MEMORY_RECALL_LOAD_QUERY_ITERATIONS=10 \
  MEMORY_RECALL_LOAD_CONCURRENCY=4 \
  MEMORY_RECALL_LOAD_VECTOR_DIMENSIONS=32 \
  MEMORY_RECALL_LOAD_VECTOR_COVERAGE_PERCENTAGES=100,50 \
  MEMORY_RECALL_LOAD_PAGINATION_LIMIT=64 \
  MEMORY_RECALL_LOAD_RESULT_BUDGET=256 \
  MEMORY_RECALL_LOAD_RELEASE=v0.0.172 \
  MEMORY_RECALL_LOAD_COMMIT=67ec81d3f5485f1865f87e265ae9f33fa15c6988 \
  MEMORY_RECALL_LOAD_PROVIDER=gcp \
  MEMORY_RECALL_LOAD_HARDWARE=cloud-sql-postgres-18-tier-name
```

The direct Go command is available for a trusted runner:

```sh
WITSELF_MEMORY_RECALL_LOAD=1 \
WITSELF_MEMORY_RECALL_LOAD_RESULTS=/trusted-artifacts/memory-recall-load.json \
go test ./internal/store \
  -run '^TestNarrativeMemoryRecallLoadPostgres$' \
  -count=1 -v -timeout 12m
```

All other controls use the `WITSELF_MEMORY_RECALL_LOAD_*` names defined in
`internal/loadquality/recall.go`. The Make target is preferred because it
records the current Git description and commit by default.

### Recall Result Contract

The retained document has schema `witself.memory-recall-load-result.v1` and
harness version `1`. Its strict, additional-properties-closed Draft 2020-12
JSON Schema is checked in at
`internal/loadquality/testdata/recall-result-schema.v1.json`; it is a new
contract and does not modify either existing result schema.

The document records UTC bounds and a pass outcome, safe environment and
PostgreSQL software metadata, the seed and complete bounded workload shape,
`OperationStats` measurements, and aggregate value-free outcomes. Every
`OperationStats` has count, wall duration, throughput, minimum, p50, p95, p99,
and maximum latency. The workload fields are `seed`, `synthetic_accounts`,
`synthetic_agents`, `cardinalities`, `query_iterations`, `concurrency`,
`vector_dimensions`, `coverage_percentages`, `pagination_limit`, and
`result_budget`. The synthetic-account count equals the number of cardinalities
and the synthetic-agent count is one greater. The measurements are:

- `cardinality_ladder[]`, with `memory_count` and `lexical_recall` stats;
- `vector_coverage[]`, with `coverage_percent`, `vector_attach`, and
  `hybrid_recall` stats; and
- `hybrid_quality`, `vector_safety`, and `pagination` stats.

The outcome objects retain these exact aggregate fields:

| Outcome | Fields |
|---|---|
| `cardinality_ladder` | `tenants`, `seeded_memories`, `recall_calls`, `all_lexical`, `all_complete` |
| `vector_coverage.cases[]` | `coverage_percent`, `eligible_memories`, `attached_vectors`, `recall_calls`, `vector_candidates`, `vector_matches`, `reported_vector_coverage`, `degraded`, `candidate_limit`, `candidate_truncated`, `hybrid_used`, `metadata_stable` |
| `vector_coverage` | `cases`, `all_profiles_listed` |
| `hybrid_quality.cases[]` | `name`, `passed`, `observed_rank`, `maximum_rank`, `vector_used`, `lexical_used`, `similarity_used` |
| `hybrid_quality` | `cases`, `recall_calls`, `score_components_verified`, `all_ranks_passed` |
| `vector_safety` | `recall_calls`, `sensitive_broad_redacted`, `sensitive_exact_owner_visible`, `cross_agent_isolated`, `cross_account_isolated`, `all_vector_queries` |
| `pagination` | `repeat_runs`, `pages_per_run`, `hits_per_run`, `recall_calls`, `result_budget`, `attached_vectors`, `vector_candidates`, `vector_matches`, `reported_vector_coverage`, `tenant_vector_fraction`, `candidate_limit`, `candidate_truncated`, `page_limits_honored`, `result_budget_reached`, `no_duplicate_ids`, `ordering_stable` |

`pages_per_run` and `hits_per_run` are two-element integer arrays, one entry for
each repeated traversal. Both runs must report
`ceil(result_budget / pagination_limit)` pages and exactly `result_budget` hits.

Validation ties every measurement count and outcome counter to the declared
workload formulas. It also requires every labeled rank, coverage relationship,
redaction/isolation assertion, and pagination assertion to pass. A partial run
therefore cannot be serialized as successful evidence. The validated document
is written atomically with mode `0600`.

In particular, the ladder performs `query_iterations` recalls per cardinality;
each coverage case performs `query_iterations` recalls and exactly the
floor-derived number of vector attachments; the three quality cases perform
`3 * query_iterations` recalls; vector safety performs four recalls; and the
two pagination traversals perform
`2 * ceil(result_budget / pagination_limit)` recalls. Validation rejects a
document whose operation counts or aggregate counters disagree with those
formulas.

The recall result never retains a DSN or endpoint identity; an account, realm,
agent, memory, vector-profile, or other resource id; query or memory text;
tags, links, content hashes, vector hashes, raw query vectors, stored vector
components, sensitive markers, or ranked-id sequences; or any prompt, token,
credential, or secret. Safe case labels and aggregate counts, ratios,
latencies, ranks, and booleans are the only workload evidence retained.

### Recall Slice Honesty Notes

- `provider` and `hardware_tier` are operator-supplied labels, not measured
  values. They must be dotless and contain only letters, digits, `+`, `_`, or
  `-`, preventing a pasted hostname from passing as a label. `release` and
  `commit` intentionally accept dots because release descriptions can require
  them.
- Summary booleans such as `all_lexical`, `metadata_stable`,
  `score_components_verified`, `all_vector_queries`, and `ordering_stable` are
  roll-ups of inline store-observing assertions. They are not later independent
  measurements. The inline assertions are the primary proof; the booleans make
  the resulting contract self-describing.
- Hybrid recall first selects a deterministic candidate universe by lexical,
  salience, recency, and id ordering, then applies vector scoring. That universe
  is hard-capped at 256 candidates. At the default largest cardinality the
  pagination workload therefore traverses only the reported 256-row snapshot
  subset and requires `candidate_truncated=true`; it does not imply exhaustive
  vector search across all 2,000 tenant memories.
- `vector_coverage` is the store-reported compatible-vector ratio inside that
  eligible candidate universe. Coverage cases run against the smallest tenant.
  For pagination, the harness attaches vectors only to the deterministic
  top-256 candidates, so reported bounded-universe coverage is `1` while
  `tenant_vector_fraction` is the exact float64 ratio
  `256 / largest_cardinality`. Neither is an estimate of an unobserved embedding
  pipeline or a claim that every memory in the largest tenant has been
  embedded.
- Retained coverage and tenant-fraction values are the exact fixture ratios.
  Inline assertions require the store's coverage metadata and the sanitized
  roll-up to equal those ratios.
- Pagination ordering is compared only across repeated identical queries in
  one disposable-schema run. Store-assigned ids are deterministic tie-breakers
  within the pinned snapshot but are not seeded, so the harness deliberately
  does not compare ranked-id sequences across independently created schemas.
- Latencies use monotonic process time, nearest-rank percentiles, and wall time
  for concurrent throughput. They are recorded, never checked against an
  absolute wall-clock threshold.

This slice advances issue #46's lexical FTS and client-supplied vector/hybrid
recall requirement at bounded tenant cardinalities. It is store-level evidence,
not an SLO or capacity claim. It does not exercise a real embedding model,
backend-generated vectors (there are none), ANN/pgvector candidate generation,
HTTP or wide-area network contention, deployed-client behavior, production
backlog, managed-cloud scaling, or semantic quality beyond the deliberately
labeled synthetic cases. In particular, it does not prove that a vector-near
memory omitted by the 256-candidate preselection can be found.

## Archive Round-Trip And Projection Slice

`TestNarrativeMemoryArchiveLoadPostgres` is the fourth executable slice. It
runs in a fresh disposable schema and uses the production store export, archive
reader, import, memory, transcript, vector, curation-relation, suspension, and
resume paths. Its synthetic fixtures are deterministic from a signed 64-bit
seed. It calls no AI, model, embedding provider, runtime client, MCP server,
secret store, or sealed-plane operation.

For each configured memory cardinality, the harness:

1. Seeds exact bounded counts of memories, versions, evidence records,
   relations, tags, transcript entries, one portable vector profile, and one
   current-head vector per memory. Seed duration is recorded, not asserted.
2. Pins a recall snapshot and runs two deterministic lexical plus two
   deterministic client-vector/hybrid queries before export.
3. System-suspends the account with the `evacuation` category, counts every
   portable table, and streams a purpose-`self`, format-version-1 tar+gzip
   archive through `ExportAccountSelf`.
4. Re-reads the complete artifact with `internal/export.Read`. A successful
   read verifies manifest/chunk ordering, newline-delimited rows, SHA-256
   checksums, bytes, row totals, and the final checksum trailer. The harness
   additionally requires the complete canonical table registry and confirms
   that `memory_versions` does not carry the generated `search_document`
   column.
5. Purges all portable rows in reverse canonical-registry order, deletes the
   account row in the same store, proves the account is absent, imports the
   suspended self archive, and compares exact exported, checksum-verified, and
   imported row counts for every manifest table before resuming the account.
6. Repeats the same pinned lexical and hybrid recalls after import. Ranked
   memory-id sequences, all score components, and retrieval metadata must be
   exactly equal. The ids and scores are compared inline and are not retained
   in the result.
7. Uses the first pre-export lexical query to prove full broad redaction of a
   sensitive memory, then proves the same redaction after import alongside
   explicit owner visibility, same-realm cross-agent isolation, and
   cross-account isolation.

The status transition is deliberate. `ExportAccountSelf` accepts active,
suspended, or closed accounts, while ordinary `ImportAccount` accepts only a
suspended or closed manifest. The harness uses the reversible suspended path;
it does not pretend that an active self archive is directly importable and it
does not permanently close the fixture account.

The retrieval projection is not a separately callable rebuild. PostgreSQL
defines `memory_versions.search_document` as a `GENERATED ALWAYS ... STORED`
`tsvector`; import omits that column and normal inserts materialize it. Exact
before/after recall equivalence is therefore the projection-rebuild proof.
Both `memory_vector_profiles` and `memory_vectors` are portable canonical
archive tables, so this slice requires hybrid equivalence, complete compatible
candidate coverage, and exact imported vector receipt/hash agreement rather
than falling back to lexical-only evidence. Import also validates each hash
against the carried vector components.

### Run The Archive Slice

Start PostgreSQL or select a dedicated test database, export its DSN through
the trusted environment, and run:

```sh
export WITSELF_TEST_DATABASE_URL='postgres://witself:witself@localhost:5432/witself?sslmode=disable'
make test-memory-archive-load
```

The result path is empty by default so the harness chooses
`/tmp/witself-memory-archive-load-<pid>.json`. Defaults and hard bounds are:

| Setting | Default | Bound |
|---|---:|---:|
| Seed | `20260831` | signed 64-bit integer |
| Memory cardinalities | `100,500,2000` | 2-5 strictly increasing values, each 10-10000 |
| Versions per memory | `2` | 2-8 |
| Evidence records per memory | `2` | 1-8 |
| Relations per memory | `1` | 1-4 |
| Tags per version | `3` | 1-16 |
| Transcript-backed memory share | `25` percent | 1-100 percent, selecting at least one memory |
| Transcript entries per selected memory | `2` | 1-8 |
| Vector dimensions | `32` | 2-4096 |

Override only bounded fixture controls and safe evidence metadata:

```sh
make test-memory-archive-load \
  MEMORY_ARCHIVE_LOAD_RESULTS=/trusted-artifacts/memory-archive-load.json \
  MEMORY_ARCHIVE_LOAD_SEED=20260831 \
  MEMORY_ARCHIVE_LOAD_CARDINALITIES=100,500,2000 \
  MEMORY_ARCHIVE_LOAD_VERSIONS_PER_MEMORY=2 \
  MEMORY_ARCHIVE_LOAD_EVIDENCE_PER_MEMORY=2 \
  MEMORY_ARCHIVE_LOAD_RELATIONS_PER_MEMORY=1 \
  MEMORY_ARCHIVE_LOAD_TAGS_PER_VERSION=3 \
  MEMORY_ARCHIVE_LOAD_TRANSCRIPT_SHARE_PERCENT=25 \
  MEMORY_ARCHIVE_LOAD_TRANSCRIPT_ENTRIES_PER_SELECTED_MEMORY=2 \
  MEMORY_ARCHIVE_LOAD_VECTOR_DIMENSIONS=32 \
  MEMORY_ARCHIVE_LOAD_RELEASE=v0.0.172 \
  MEMORY_ARCHIVE_LOAD_COMMIT=67ec81d3f5485f1865f87e265ae9f33fa15c6988 \
  MEMORY_ARCHIVE_LOAD_PROVIDER=gcp \
  MEMORY_ARCHIVE_LOAD_HARDWARE=cloud-sql-postgres-18-tier-name
```

The direct Go command is:

```sh
WITSELF_MEMORY_ARCHIVE_LOAD=1 \
WITSELF_MEMORY_ARCHIVE_LOAD_RESULTS=/trusted-artifacts/memory-archive-load.json \
go test ./internal/store \
  -run '^TestNarrativeMemoryArchiveLoadPostgres$' \
  -count=1 -v -timeout 15m
```

Every multi-rung workload budgets one two-minute context deadline per
cardinality rung (a three-rung ladder gets six minutes), and the whole
harness has a twelve-minute guard. Those are operational runaway bounds, not performance
assertions or SLOs. Provider and hardware tier must be dotless labels containing
only letters, digits, `+`, `_`, or `-`; release and commit metadata may contain
dots.

### Archive Result Contract

The retained document has schema `witself.memory-archive-load-result.v1` and
harness version `1`. Its separate, additional-properties-closed Draft 2020-12
schema is `internal/loadquality/testdata/archive-result-schema.v1.json`; none of
the first three result schemas is modified.

Each cardinality retains:

- `OperationStats` for seed, export, full verification, import, lexical recall
  before and after, hybrid recall before and after, and post-import safety;
- export/import row and byte throughput, archive and verified-chunk bytes,
  manifest format/schema/purpose/status, chunk/table/non-empty-table counts,
  and exact row totals;
- the complete lexicographically sorted 74-table v1 registry with exported,
  verified, and imported counts, including explicit zero-row tables;
- focal counts for memories, versions, evidence, relations, transcript
  conversations/entries, vector profiles/vectors, and JSON tag assignments;
- four value-free recall-equivalence cases with hit counts and exact ranking,
  score-component, and metadata assertions; and
- value-free archive-integrity, same-store, vector-portability, sensitive-
  redaction, and isolation assertions.

Validation ties every count to the declared workload formula: one seed,
export, verification, and import measurement per cardinality; two lexical and
two hybrid calls both before and after; four safety calls; all configured
cardinalities; the complete pinned registry; exact per-table and focal counts;
and all inline correctness assertions. A partial run cannot serialize a pass.
Summary booleans are only roll-ups of the inline store/archive comparisons; the
comparisons are the proof. In particular, `sensitive_broad_redacted` rolls up
both pre-export and post-import full-field redaction checks, while vector
round-trip rolls up exact profile fields, receipt hashes, dimensions, timestamps,
row counts, and hybrid vector-use/coverage assertions.

Pinned `AsOf`, change-sequence, and deleted-memory-count coordinates make all
returned score components deterministic within one round trip, so the contract
uses exact float equality and a documented tolerance of zero. The harness does
not compare timings to an absolute threshold. It is local store-level evidence,
not a large-archive SLO, managed-cloud capacity claim, rollback drill, or proof
of behavior across different schemas or releases.

## Concurrent Agents And Tenant-Isolation Slice

`TestNarrativeMemoryConcurrencyLoadPostgres` is the fifth executable slice. It
runs directly against one PostgreSQL endpoint in a fresh disposable schema,
applies the complete migration set, creates a bounded synthetic
account/realm/agent fleet, and drops the complete schema during cleanup. A
signed 64-bit seed determines every harness-generated fixture value and canary;
store-assigned ids and wall-clock time are never random seeds.

The workload mutations and memory/curation operations call only production
store methods. Test-only read-only SQL records PostgreSQL version metadata and
checks the complete curation cursor table. The harness performs no inference
and calls no AI or model, embedding service, runtime client, MCP server, secret
store, or sealed-plane operation. It creates no agent token. Curation winners
use the exact empty plan, so this slice measures deterministic queue, fencing,
and cursor behavior without pretending to evaluate client curation quality.

### What The Concurrency Harness Proves

The bounded workload exercises five related cases across the complete synthetic
fleet:

1. **Multi-tenant topology.** The harness creates the configured accounts,
   realms per account, and agents per realm. Every principal receives exactly
   `seed_memories_per_agent` non-sensitive canary memories plus one sensitive
   memory. Canary and sensitive markers are deterministic and unique to their
   principal, and every returned fixture is asserted against its exact expected
   value before aggregate counters are recorded.
2. **Concurrent mixed operations.** Every agent launches
   `workers_per_agent` workers as part of one whole-fleet start. Each worker
   performs `operations_per_worker` capture/lexical-recall/adjust batches. Every
   recall must return exactly one expected row, not merely a non-empty page, and
   every hit is checked for the exact calling account, realm, owner kind, and
   owner id. Capture and adjustment receipts are also compared to their exact
   expected versions and values. An all-worker-and-probe readiness gate pins the
   whole-fleet release. After that release, every probe signals that it has begun
   before the coordinator releases the mixed workers from their second gate.
3. **Isolation under load.** Dedicated probes are launched in the same phase as
   the mixed workers. Each principal and iteration performs one broad recall, three
   one-row owner-control recalls, and one cross-account, one cross-realm, and
   one cross-agent recall. The broad recall must return the exact seeded count:
   all non-sensitive canaries plus one fully redacted sensitive row. Every
   owner control must return exactly one expected row, and every corresponding
   foreign query must return zero rows. Every returned content value is scanned
   against every synthetic foreign canary and sensitive marker. Overlap is
   measured per probe store call via an atomic in-flight mixed-operation counter,
   retained as `overlap_operation_samples`, structurally biased by the
   workers-wait-for-probes-begun rendezvous, and required `>=1` only when
   isolation iterations `>=2`.
4. **Concurrent curation claims.** Every principal enqueues one request. Three
   foreign principals probe that request across the account, realm, and agent
   boundaries and must receive the typed not-found refusal. The configured
   owner claim workers then race each request across the fleet; exactly one wins
   and all other owner attempts receive the expected busy refusal. Each winner
   applies the reviewed empty plan, advances exactly its own cursor, and cannot
   advance a foreign owner's cursor.
5. **Sensitive fan-out.** One selected owner's exact sensitive fixture content
   is used as the lexical query for every principal. The owner, with sensitive
   inclusion explicitly enabled, must receive exactly one exact-value hit.
   Every other principal receives zero rows, even though each uses that same
   targeted query.

Each operation family retains `OperationStats`: count, wall duration,
throughput, minimum, p50, p95, p99, and maximum latency. Concurrent families use
their real phase or subphase wall time rather than the sum of individual call
durations. The seed family passes the sum of its measured capture durations as
its wall interval, and the sequential curation-apply family likewise sums only
its measured apply durations. Curation claim sums its two concurrent subphase
walls; sensitive fan-out sums the measured owner-call duration and foreign
concurrent wall. Timings use monotonic process time and nearest-rank percentiles.

### Run The Concurrency Slice

Keep the DSN in the trusted parent environment and invoke the dedicated target:

```sh
export WITSELF_TEST_DATABASE_URL='postgres://witself:witself@localhost:5432/witself?sslmode=disable'
make test-memory-concurrency-load
```

The result path is empty by default so the harness chooses
`/tmp/witself-memory-concurrency-load-<pid>.json`. Concurrent invocations
therefore cannot atomically rename over one another. Defaults and hard bounds
are:

| Setting | Default | Allowed range |
|---|---:|---:|
| Seed | `20260901` | signed 64-bit integer |
| Synthetic accounts | `4` | `2..8` |
| Realms per account | `2` | `2..4` |
| Agents per realm | `4` | `2..8` |
| Non-sensitive seed memories per agent | `4` | `1..64` |
| Mixed-operation workers per agent | `2` | `2..8`, no greater than seed memories per agent |
| Capture/recall/adjust batches per worker | `2` | `1..50` |
| Isolation probe iterations per agent | `2` | `1..50` |
| Owner claim workers per request | `4` | `2..32` |

The default topology contains 8 realms and 32 principals. The largest accepted
topology contains 32 realms and 256 principals. Override only bounded workload
controls and safe evidence metadata:

```sh
make test-memory-concurrency-load \
  MEMORY_CONCURRENCY_LOAD_RESULTS=/trusted-artifacts/memory-concurrency-load.json \
  MEMORY_CONCURRENCY_LOAD_SEED=20260901 \
  MEMORY_CONCURRENCY_LOAD_ACCOUNTS=4 \
  MEMORY_CONCURRENCY_LOAD_REALMS_PER_ACCOUNT=2 \
  MEMORY_CONCURRENCY_LOAD_AGENTS_PER_REALM=4 \
  MEMORY_CONCURRENCY_LOAD_SEED_MEMORIES_PER_AGENT=4 \
  MEMORY_CONCURRENCY_LOAD_WORKERS_PER_AGENT=2 \
  MEMORY_CONCURRENCY_LOAD_OPERATIONS_PER_WORKER=2 \
  MEMORY_CONCURRENCY_LOAD_ISOLATION_ITERATIONS=2 \
  MEMORY_CONCURRENCY_LOAD_CLAIM_WORKERS=4 \
  MEMORY_CONCURRENCY_LOAD_RELEASE=v0.0.172 \
  MEMORY_CONCURRENCY_LOAD_COMMIT=67ec81d3f5485f1865f87e265ae9f33fa15c6988 \
  MEMORY_CONCURRENCY_LOAD_PROVIDER=gcp \
  MEMORY_CONCURRENCY_LOAD_HARDWARE=cloud-sql-postgres-18-tier-name
```

The direct Go command is available for a trusted runner:

```sh
WITSELF_MEMORY_CONCURRENCY_LOAD=1 \
WITSELF_MEMORY_CONCURRENCY_LOAD_RESULTS=/trusted-artifacts/memory-concurrency-load.json \
go test ./internal/store \
  -run '^TestNarrativeMemoryConcurrencyLoadPostgres$' \
  -count=1 -v -timeout 5h
```

All controls use the `WITSELF_MEMORY_CONCURRENCY_LOAD_*` names defined in
`internal/loadquality/concurrency.go`; the Make variables above forward to
those names. The Make target is preferred because it records the current Git
description and commit by default. `WITSELF_TEST_DATABASE_URL` is deliberately
not a Make variable and is never part of the result contract.

Deadlines are proportional to the selected topology. One fixed agent batch is
eight principals, so the budget-unit count is
`ceil(synthetic_principals / 8)`. Every phase that scales across agents receives
that many `ConcurrencyAgentBatchDeadline` units, and one unit is two minutes.
The default 32-principal fleet therefore receives four units, or eight minutes,
for each scaling phase; the maximum 256-principal fleet receives 32 units, or
64 minutes. The overall driver context is derived from all phase budgets plus a
setup guard, and the Make target's five-hour timeout is a separate outer guard.
These are runaway bounds, not latency thresholds or SLOs.

Claimed curation runs have a 30-minute maximum lease, while the proportional
curation phase can be longer. Preparation therefore runs in fixed
`ConcurrencyAgentBatchSize` batches of eight jobs. Immediately before every
preparation batch, the harness renews the complete live job set with a fresh
batch epoch; no gap between those renewals includes more than one batch's work.
It renews the complete set once more immediately before building `knownCursors`
and reading the initial cursor snapshot. During sequential apply, it retains the
existing cadence that renews the remaining unapplied suffix immediately before
each eight-job apply batch.

### Concurrency Result Contract

The retained document has schema
`witself.memory-concurrency-load-result.v1` and harness version `1`. Its
separate, additional-properties-closed Draft 2020-12 JSON Schema is
`internal/loadquality/testdata/concurrency-result-schema.v1.json`, with `$id`
`https://witself.witwave.ai/schemas/memory-concurrency-load-result.v1.schema.json`.
None of the first four result schemas is modified.

The workload records `seed`, `synthetic_accounts`, `realms_per_account`,
`agents_per_realm`, `synthetic_realms`, `synthetic_principals`,
`seed_memories_per_agent`, `workers_per_agent`, `operations_per_worker`,
`isolation_iterations`, and `claim_workers`. The measurements are exactly
`seed`, `mixed_capture`, `mixed_recall`, `mixed_adjust`, `isolation_probe`,
`curation_request`, `curation_claim`, `curation_apply`, and
`sensitive_fanout`.

The outcome objects retain these exact aggregate fields:

| Outcome | Fields |
|---|---|
| `topology` | `accounts`, `realms`, `principals`, `canary_memories`, `sensitive_memories`, `seeded_memories`, `all_principals_seeded`, `all_canaries_unique`, `all_sensitive_seeded` |
| `mixed_operations` | `workers`, `operation_batches`, `capture_calls`, `recall_calls`, `adjust_calls`, `recall_hits`, `owner_checks`, `foreign_hits`, `overlap_operation_samples`, `exact_recall_values`, `exact_adjust_values`, `all_hits_exact_owner`, `whole_fleet_start_synchronized`, `all_operations_complete` |
| `isolation` | `probe_agents`, `probe_rounds`, `broad_recall_calls`, `broad_hits`, `broad_visible_canaries`, `broad_sensitive_redactions`, `own_control_recall_calls`, `own_control_hits`, `cross_account_recall_calls`, `cross_realm_recall_calls`, `cross_agent_recall_calls`, `marker_scans`, `foreign_hits`, `foreign_canary_hits`, `sensitive_content_hits`, `broad_counts_exact`, `own_counts_exact`, `all_hits_exact_owner`, `no_foreign_canaries`, `no_sensitive_content`, `cross_account_isolated`, `cross_realm_isolated`, `cross_agent_isolated` |
| `curation_claims` | `requests`, `request_calls`, `owner_claim_attempts`, `owner_claim_wins`, `owner_claim_losses`, `foreign_claim_attempts`, `cross_account_refusals`, `cross_realm_refusals`, `cross_agent_refusals`, `typed_foreign_refusals`, `foreign_claim_wins`, `apply_calls`, `owner_cursor_advances`, `foreign_cursor_advances`, `single_winner_per_request`, `all_foreign_claims_typed`, `only_owner_cursor_advanced`, `all_requests_applied` |
| `sensitive_fanout` | `query_calls`, `owner_query_calls`, `foreign_query_calls`, `owner_hits`, `foreign_hits`, `sensitive_content_leaks`, `owner_exact_read_succeeded`, `all_foreign_queries_isolated` |

Validation uses these exact formulas. Let `P = accounts * realms_per_account *
agents_per_realm`, `M = seed_memories_per_agent`, `W = workers_per_agent`,
`O = operations_per_worker`, `I = isolation_iterations`, and
`C = claim_workers`:

- topology creates `P*M` canary memories, `P` sensitive memories, and
  `P*(M+1)` total seed captures;
- mixed work launches `P*W` workers and performs `P*W*O` capture calls,
  lexical recalls, adjustments, exact recall hits, and per-hit owner checks;
- isolation performs `P*I` probe rounds and `7*P*I` measured recalls. Those
  rounds must yield exactly `P*I*(M+1)` broad hits, `P*I*M` visible canaries,
  `P*I` sensitive redactions, `3*P*I` one-hit owner controls, and `P*I`
  zero-hit probes for each of the three foreign dimensions. The marker-scan
  count is exactly broad hits plus owner-control hits. The retained overlap
  sample count is in `0..7*P*I`, and it must be at least one when `I >= 2`;
- curation performs `P` request calls, `P*C` owner claim attempts, `P` wins,
  `P*(C-1)` owner losses, `3*P` typed foreign claim attempts, and `P` applies
  and owner cursor advances. The claim measurement count is `P*(C+3)`; foreign
  claim wins and foreign cursor advances must both remain zero; and
- sensitive fan-out performs `P` queries: one owner query with one exact hit
  and `P-1` foreign queries with zero hits.

Every measurement count and aggregate counter must satisfy those formulas, all
foreign-hit and sensitive-leak counters must be zero, and every required inline
assertion must pass before a `pass` document can be serialized. A partial run
therefore cannot produce passing evidence. The validated document is written
atomically with mode `0600`.

The concurrency result never retains a DSN or endpoint identity; an account,
realm, agent, memory, request, run, cursor, receipt, or other store/workload
resource id; an idempotency or coalescing key; query text, memory, tag, canary,
or sensitive-marker content; a vector or embedding payload; a content or plan
hash; or any prompt, token, credential, or secret. Only bounded workload
dimensions, aggregate counters, safe environment metadata (including the
explicitly supplied release and commit labels), operation statistics, and
assertion roll-ups are retained.

### Verified Store-Scoping Premises

The harness relies on and directly checks these production-store properties:

- `RecallMemories` accepts only an agent principal, verifies that the supplied
  account/realm/agent tuple is live, and pins its snapshot watermark to that
  same owner lane.
- Both lexical candidate selection and returned-payload loading constrain
  `account_id`, `realm_id`, `owner_kind='agent'`, and `owner_id`. The harness
  nevertheless compares all four returned owner coordinates on every hit; it
  does not treat the SQL predicate alone as evidence.
- Broad recall does not exclude an owner's sensitive row by default. It can
  return the row with caller-authored value fields cleared and `redacted=true`.
  That is why the exact broad count is `M+1`, while the exact visible-canary
  count is `M` and the sensitive-redaction count is one per probe round.
- `IncludeSensitive=true` changes visibility only inside the caller's already
  scoped owner lane. It cannot select a different account, realm, or agent. The
  sensitive fan-out case checks both directions with an exact owner hit and
  exact zero foreign hits.
- Curation request and run loads bind ids to the caller's account, realm, and
  owner lane. A foreign request id therefore returns the typed
  `ErrMemoryCurationNotFound` refusal rather than disclosing or claiming the
  request. Expected owner contention losses are separately asserted as
  `ErrMemoryCurationBusy`.
- Curation cursor inputs and cursor updates carry the same account, realm, and
  owner predicates. The harness maps each apply receipt to the corresponding
  owner and checks the complete cursor table so an owner-only success cannot
  hide a foreign advance.

### Concurrency Slice Honesty Notes

- `provider` and `hardware_tier` are operator-supplied labels, not measured
  values. They must be dotless and contain only letters, digits, `+`, `_`, or
  `-`, preventing a pasted hostname from passing as a label. `release` and
  `commit` may contain dots.
- Summary booleans such as `all_hits_exact_owner`, `broad_counts_exact`,
  `whole_fleet_start_synchronized`, `all_foreign_claims_typed`,
  `only_owner_cursor_advanced`, and `all_foreign_queries_isolated` are roll-ups
  of inline coordination or store-observing assertions. Each represented
  condition has a fail-fast path before evidence is emitted; they are not later
  independent measurements. `whole_fleet_start_synchronized` specifically
  requires every worker and probe to reach the shared readiness gate, no
  participant to cross it before coordinator release, and every participant to
  cross it afterward. Exact returned values and owner tuples, typed `errors.Is`
  checks, winner maps, full marker scans, and cursor-table comparisons are the
  primary proof.
- `overlap_operation_samples` is a direct counter, not a summary boolean.
  Overlap is measured per probe store call via an atomic in-flight
  mixed-operation counter, retained as `overlap_operation_samples`,
  structurally biased by the workers-wait-for-probes-begun rendezvous, and
  required `>=1` only when isolation iterations `>=2`.
- Winner input paging (`GetCurationRunInputs`, in 200-row pages), empty-plan
  submission (`PlanCuration`), stored-plan read-back (`GetCurationPlan`), the
  per-batch fenced lease renewals from preparation and apply, the additional
  pre-cursor-snapshot renewal (`RenewCuration`), and read-only full cursor-table
  audits are lease/validation housekeeping, not named workload measurements.
  They remain inside the proportional curation phase deadline but are excluded
  from named-operation latency and throughput.
- "Whole-fleet concurrency" means all configured synthetic principals and
  workers contend through one test process, store pool, PostgreSQL endpoint,
  and disposable schema. It does not simulate multiple deployed client
  processes, HTTP/MCP transport, wide-area networking, failover, connection
  pool diversity, or production background workers.
- The exact marker scans cover the synthetic content values returned by these
  recalls. They do not prove absence from native PostgreSQL errors, database
  logs, backups, metrics, side channels, or unrelated interfaces.
- This is model-free store correctness and timing evidence. It does not measure
  semantic recall usefulness, client curation quality, model tokens or cost,
  a real embedding pipeline, managed-cloud capacity, or a production SLO. No
  production threshold or default should be inferred from a local pass.

## Evidence Checklist

Retain the JSON result with:

- the immutable release tag and full commit SHA being tested;
- PostgreSQL provider/version and non-sensitive hardware tier;
- runner hardware and network placement notes outside the JSON when needed;
- the command/workflow URL and timestamp; and
- any operator-approved exception.

For lexical comparisons, use the same seed and corpus digest. For curation
comparisons, use the same seed and complete workload shape. For recall
comparisons, use the same seed, cardinality ladder, iteration/concurrency shape,
vector dimensions and coverage percentages, and pagination shape. Never compare
ranked ids across independently created schemas. For archive comparisons, use
the same seed, complete cardinality/focal-count shape, vector dimensions, and
canonical result-contract version; the exact ranked-id comparison occurs only
inside each same-account round trip. For concurrency comparisons, use the same
seed, complete account/realm/agent topology, seed-memory count, worker and
operation counts, isolation iterations, claim-worker count, and result-contract
version. Change one workload dimension at a time. A dirty checkout should be
labeled exploratory, not a release baseline.

## What Still Remains For Issue #46

These five slices intentionally do **not** claim production readiness. Issue #46
still requires:

1. Broader production instrumentation. Bounded HTTP, memory operation/recall,
   vector coverage/fallback, and curation domain-call metrics are implemented.
   Durable run-transition and lease-event metrics, queue-age distributions,
   broader archive/rebuild timing, remaining operation coverage, dashboards,
   alerts, and measured defaults
   still remain.
2. Queue and curation load beyond this bounded local slice: rollback under
   contention, production queue/backlog-age distributions, larger and more
   concurrent shapes, managed-cloud repetitions, and reviewed safe limits/SLOs.
3. Managed-cloud and larger client-vector/hybrid repetitions, explicit
   zero-compatible-vector lexical fallback, ANN/projection scale, and reviewed
   tuning beyond the fixed 256-candidate correctness baseline. The recall and
   archive slices cover deterministic bounded degradation and same-account
   import projection materialization, not those production-scale questions.
4. Archive rollback/failure-injection drills, concurrently mutating export
   sources beyond the snapshot contract, cross-release compatibility matrices,
   and representative managed-cloud large-archive durations. The fourth slice
   covers successful complete same-store round trips only.
5. Larger high-cardinality shapes for versions, evidence, relations,
   transcripts, archive bytes, and concurrency beyond the fifth slice's bounded
   256-principal topology. Current ceilings are fixture and correctness guards,
   not capacity claims.
6. A richer adjudicated relevance corpus with false-positive and ranking
   metrics beyond the two exact lexical and three constructed hybrid cases in
   the current harnesses.
7. Client-side curation quality, duplicate growth, supersession quality,
   summarization drift, and model token/cost envelopes. Those require explicit
   client inference and remain outside this model-free store harness.
8. Managed-cloud baselines on representative hardware, documented production
   SLOs/alerts/safe limits, degraded-mode drills, and measured default tuning.
9. A protected repeatable workflow that uploads these sanitized results and
   identifies the release, PostgreSQL tier, and runner without exposing
   credentials.

No production default should be changed from any local result. Production
defaults and thresholds require repeated GCP/AWS/Azure measurements and an
explicit review of the retained evidence.
