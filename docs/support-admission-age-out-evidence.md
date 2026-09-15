# Support admission and age-out evidence

On 2026-09-07, focused local PostgreSQL and boundary verification passed for implementation commit `daaea0fe08f38633ab6306a387862badc3ae6193` (tree `2640f4b9382c4f3f7c7a0d3bb89f1b82edc8e3d3`). This identifies the implementation tested before this document and its catalog update were added. The run reused ten existing tests across four packages; no production behavior or test source changed.

All four selected Go commands exited **0**. Complete JSON streams contain **ten unique top-level run/pass pairs across four passing packages**, with no subtests, failures, skips, missing, duplicate or unexpected selected cases. All 30 recorded commands and the driver/outer execution exited 0, without timeout or surviving process groups. The complete source, runner and execution evidence received independent review.

| Supported claim | Existing implementation and focused evidence |
| --- | --- |
| Ticket creation shares an account quota across operators; refusal leaves ticket, message and audit row counts unchanged. | [Admission implementation](../internal/store/support_limits.go) and `TestSupportTicketRateLimitPostgres`: eight concurrent attempts from two operators at a configured limit of three require three admissions and five refusals. Another account remains available, a foreign operator is rejected, replies remain possible and admission resumes after the window expires. |
| The account limiter uses the email channel's 10-per-minute default and rejects unbounded configuration. | Default/configuration tests in [store](../internal/store/support_limits_test.go) and [server command](../cmd/witself-server/support_limits_test.go). The PostgreSQL test also directly seeds 1,000 tickets and requires a bounded typed refusal; this is not an archive-import or query-plan test. |
| Store/server mapping and HTTP 429 expose retry information; the Go client preserves the refusal code and retryable flag. | [HTTP and client test](../internal/server/support_limits_test.go) and `TestMapSupportRateLimitError` check typed retry detail, HTTP `rate_limited`, `retryable`, retry delay, `Retry-After` and exclusion of fixture private text from the response. The client assertion checks its code and retryable flag, using an in-process handler adapter. |
| Age-out resolves eligible stale tickets in bounded batches while retaining their threads and allowing customer reopening. | [Age-out implementation](../internal/store/support_age_out.go) and [PostgreSQL test](../internal/store/support_age_out_test.go) use batch size one, account/ticket locks, cutoff equality and one-microsecond exclusion, inactive-account exclusion, state and human-reply eligibility, audit/message counts, resolution timestamps and customer reopening. |
| Worker attempts have a deadline, retry after a failed batch and stop on cancellation. | `TestSupportTicketAgeOutWorkerRetriesAndCancels` injects the resolver and wait functions; it observes one failed and one successful attempt. It does not launch a deployed worker. |
| Age-out registration remains disabled by default and opt-in configuration is bounded. | Store configuration tests and [worker configuration/registration test](../cmd/witself-worker/support_age_out_test.go) check disabled registration, explicit opt-in and invalid settings. Defaults remain 30 days, at most 100 tickets per hourly sweep and a 10-second batch deadline; the database fixture uses batch size one. |

The exact selections below use the portable `go` executable name. Execution used the pinned Go 1.26.6 binary on darwin/arm64, `GOFLAGS='-p=2 -mod=readonly'`, `GOMAXPROCS=2`, `GOTOOLCHAIN=local`, `GOENV=off`, `GOWORK=off`, `GOPROXY=off`, `GOSUMDB=off`, `WITSELF_TEST_REQUIRE_DATABASE=1` and `WITSELF_TEST_REQUIRE_NODE=1`. Each command had a 900-second outer deadline. Existing 30-second database-operation contexts and adaptive schema-cleanup deadlines were unchanged.

```sh
go test -count=1 -race -shuffle=on -json -timeout=10m -run '^(TestSupportTicketRateLimitDefaultsMatchEmailChannel|TestSupportTicketRateLimitConfigBounds|TestSupportTicketRateLimitPostgres|TestSupportTicketAgeOutConfigBounds|TestSupportTicketAgeOutWorkerRetriesAndCancels|TestSupportTicketAgeOutPostgres)$' ./internal/store

go test -count=1 -race -shuffle=on -json -timeout=10m -run '^(TestOpenSupportTicketRateLimitHTTPAndClient)$' ./internal/server

go test -count=1 -race -shuffle=on -json -timeout=10m -run '^(TestSupportTicketRateLimitConfigFromEnv|TestMapSupportRateLimitError)$' ./cmd/witself-server

go test -count=1 -race -shuffle=on -json -timeout=10m -run '^(TestSupportTicketAgeOutConfigFromEnv)$' ./cmd/witself-worker
```

The run used a uniquely owned local PostgreSQL database, confirmed absent before creation and empty afterward. PostgreSQL was `16.14 (Debian 16.14-1.pgdg13+1)`. Each existing database fixture created its own schema and called `Store.Migrate()`. Both named test streams contain all **87 frozen migration filenames exactly once**, followed by version **95**; the production migration path independently checks the database version against its compiled target before returning success. Migration tree: `d7c01724634b4bda9270489d091617a1130ef9a5`. This is fixture migration output plus successful version-checking code, not a post-cleanup Goose-table query or focused migration-recovery certification.

The final fixture query found zero non-system tables and zero non-system schemas other than public. The driver dropped the owned database and verified absence; a separate root query also returned zero. Recorded process groups, the driver, scratch directory and shared lock were independently checked absent. All **1,645** tracked source files remained unchanged, and pre-existing checkout work was preserved.

| Package | Named passes | JSON records | Observed shuffle seed |
| --- | ---: | ---: | --- |
| `internal/store` | 6 | 205 | `1788816440186832000` |
| `internal/server` | 1 | 9 | `1788816461093994000` |
| `cmd/witself-server` | 2 | 13 | `1788816467579239000` |
| `cmd/witself-worker` | 1 | 9 | `1788816470342645000` |

The following SHA-256 digests identify complete retained private evidence. Raw output remains private; these identifiers retain provenance without publishing ticket/account content, connection details or local paths.

| Record | SHA-256 |
| --- | --- |
| Frozen source baseline and identical before/after snapshots, 1,645 files | `b968d4436eb9364a89841e482843771a245b2907c020334a8fe5a7469ceeee65` |
| Toolchain baseline | `11b2c91f23a7f59c5751b1a9e394e999955aab85f3bcbc03c05a13ecf0e662b9` |
| Reviewed external focused runner | `3d7a53dc563cae7a39ebc1a787d11ff1e5d6f1b37ca3de1cfb923a83d5eb4cee` |
| Complete store JSON stream, 47,495 bytes | `8ce29085658d3e7771647eaa7f444fedf5d6ebe8feb950a7744070558bc0cd02` |
| Complete server JSON stream, 1,547 bytes | `2e9ed82b14c71f9e5411e4f5847b111674d84381705694da5609602374ff7ee7` |
| Complete server-command JSON stream, 2,293 bytes | `c48c83b6719c1a93ad765aaf19a182a52914644fa853682cee574aaffa0796cc` |
| Complete worker-command JSON stream, 1,535 bytes | `2b2dcc03fb0990ed031cd99ab0901377519aaac5eb07fe52935f9e4a24252bb6` |
| Exact named outcome and migration inventory | `9c0bf227e7b369c48b7f1b138efee18f52015ed4ba6d561d77b75e5ad94ae3c9` |
| Complete focused result and command/log manifest | `a55e0ffa00225d4cbc0df59aeb95cd1367663150751cdee065a66d70f4b0d0a5` |
| Actual outer tool exit | `6368d6c20bb7a5d6ae5fc4aac2221c6393275e67b442dd73c92c66e99cde9aee` |
| Independent root outcome and database-absence verification | `500a793f2c9c10f3d2769e3e35f2e8cc50b559ef96902c2b29164ed9a454db8a` |
| Independent complete focused evidence review | `58a7654e38abf8e8cc8eb297aa0c78e43606a8a4373bcc42c4f38cc8449a63c9` |

These local fixtures support retained validation of admission and age-out. They do not certify multi-replica deployment, SQL scan cost, production load, CLI execution, real customer intake, escalation or SLA delivery, evidence attachments, content deletion or general retention behavior. The [support policy](support-policy.md), human first-response promise, parked email intake and AI responder are unchanged. The composite support abuse/retention gate remains open for bounded evidence-attachment handling; the separate rollout canary remains open. Managed support stays at five of seven passing gates, with conditional readiness and limited rollout.
