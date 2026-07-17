# Narrative Memory Load And Quality Baseline

Status: first executable PostgreSQL slice. This runbook defines a reproducible,
opt-in lexical-memory baseline for production-readiness issue
[#46](https://github.com/witwave-ai/witself/issues/46). It is useful evidence,
but it does not by itself close that issue; the remaining gates are listed
below.

## What This Harness Proves

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

## Run It

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

## Result Contract

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

## Evidence Checklist

Retain the JSON result with:

- the immutable release tag and full commit SHA being tested;
- PostgreSQL provider/version and non-sensitive hardware tier;
- runner hardware and network placement notes outside the JSON when needed;
- the command/workflow URL and timestamp; and
- any operator-approved exception.

Use the same seed and corpus digest when comparing releases. Change one workload
dimension at a time. A dirty checkout should be labeled exploratory, not a
release baseline.

## What Still Remains For Issue #46

This first slice intentionally does **not** claim production readiness. Issue
#46 still requires:

1. Broader production instrumentation. Bounded HTTP, memory operation/recall,
   vector coverage/fallback, and curation domain-call metrics are implemented.
   Durable run-transition and lease-event metrics, queue-age distributions,
   archive/rebuild timing,
   remaining operation coverage, dashboards, alerts, and measured defaults
   still remain.
2. Queue and curation load: coalescing, claims, leases, fencing, expiry,
   retries, bounded input paging, plan/apply conflicts, rollback, and backlog
   age.
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

No default should be changed from this first local result. Production defaults
and thresholds require repeated GCP/AWS/Azure measurements and an explicit
review of the retained evidence.
