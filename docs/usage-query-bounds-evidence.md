# Usage query bounds evidence

On 2026-09-07, focused local PostgreSQL and client-boundary verification passed for implementation commit `a45bf59b6fa1277dc524501fd1c5e3bbc492dad3` (tree `5fb0c49ea122a9596766a0a428068524a5219837`). This is the implementation tested before this evidence document and its catalog update were added. The verification reused existing tests; it introduced no production behavior or test changes.

Both selected Go commands exited **0**. Complete JSON streams contained **19 top-level tests and 45 subtests: 64 named run/pass pairs across five passing packages**, with no failure, skip, missing, unexpected or duplicate selected case. The store stream had 264 JSON records; the boundary stream had 108. All 13 validation/setup/cleanup child commands, the wrapper and its outer execution exited 0, without timeout or unresolved child process groups. The source and evidence received independent review.

| Supported claim | Implementation and existing evidence |
| --- | --- |
| Reports return at most 10,000 points in bucket/dimension/unit order; totals cover returned points. An extra match requires explicit truncation opt-in. | [Store query](../internal/store/usage.go), exact-cap/cap-plus-one and high-cardinality PostgreSQL tests. The latter exercises 12,960 canonical hourly points and a complete 2,160-point filtered result. |
| UTC-normalized windows have an inclusive start and exclusive end, with maximum durations of 90 days hourly and 1,830 days daily. | Store normalization/window tests and PostgreSQL filtering and oversized-window checks. |
| New event ingestion and queries use a closed 16-dimension vocabulary; reports exclude unknown historical stored dimensions. | Literal vocabulary and ingestion checks, duplicate-filter normalization and the unknown stored-dimension PostgreSQL case. Vocabulary membership alone does not establish event emission. |
| Archive import preserves historically valid dimension syntax, including dimensions outside the current ingestion vocabulary; malformed dimensions are rejected. | [Import validation](../internal/store/import.go), syntax cases for both usage tables and actual legacy-dimension export/import/re-export with exact row preservation. |
| Real archive import rejects malformed usage dimensions and supports internally consistent high-cardinality archives while report bounds remain effective. | The usage round-trip's three `hostile_import_*` cases exercise actual import, malformed-import rollback and post-import cap/opt-in behavior. Direct database seeding is complementary evidence. |
| Agent-scoped HTTP usage refuses unnegotiated partial results with HTTP 422 and no usage payload; truncation opt-in must be explicit and unambiguous. | [HTTP handler](../internal/server/server.go), scope/filter, typed truncation, opt-in and adapter tests. |
| The client does not opt in by default and rejects unnegotiated truncation; CLI opt-in warns that totals cover returned points, including JSON output. | [Client](../internal/client/usage.go), client refusal/decoding tests and [CLI](../cmd/witself/main.go) warning/refusal tests. |

The two actual test selections are reproduced below with the portable `go` executable name. Execution used the pinned Go 1.26.6 binary on darwin/arm64, `GOFLAGS='-p=2 -mod=readonly'`, `GOMAXPROCS=2`, `GOTOOLCHAIN=local`, `GOENV=off`, `GOWORK=off`, `GOPROXY=off`, `GOSUMDB=off`, `WITSELF_TEST_REQUIRE_DATABASE=1` and `WITSELF_TEST_REQUIRE_NODE=1`. Each command had a 900-second outer bound. Existing one-minute store fixture contexts were unchanged.

```sh
go test -json -race -shuffle=on -count=1 -timeout=10m -run '^(TestNormalizeUsageQuery|TestValidateUsageEventInput|TestUsageQueryWindowBoundaries|TestUsageDimensionVocabularyIsClosed|TestImportedUsagePreservesHistoricalDimensionSyntax|TestUsagePostgresRoundTrip|TestUsagePostgresRowLimitAndHostileImportedCardinality|TestUsagePostgresHighCardinality|TestImportUsageLegacyDimensionRoundTripPostgres)$' ./internal/store

go test -json -race -shuffle=on -count=1 -timeout=10m -run '^(TestUsageIsAgentScopedAndParsesFilters|TestUsageTruncationContract|TestUsageRequiresTruncationOptIn|TestUsageDecodesTruncation|TestUsageDoesNotOptInByDefault|TestUsageQueryTooLargePreservesServerMessage|TestUsageRejectsUnnegotiatedTruncation|TestUsageCommandShowsTruncation|TestUsageCommandFailsWithoutTruncationOptIn|TestUsageReportPreservesTruncation)$' ./internal/server ./internal/client ./cmd/witself ./cmd/witself-server
```

The commands require an explicitly bound, owned disposable PostgreSQL database. The run set the required-database flag and independently rejected absent or skipped named tests, including fixtures whose inline missing-database skip does not consult that flag. PostgreSQL was `16.14 (Debian 16.14-1.pgdg13+1)`. The database was independently confirmed absent before creation and empty after creation; selected fixtures then used their existing `Store.Migrate()` calls. Final applied versions were exactly `0–41, 50–95`: the initial version plus all 87 frozen SQL migrations, at schema **95**, migration tree `d7c01724634b4bda9270489d091617a1130ef9a5`.

All nested outcomes are enumerated here. Each row expands as the parent name followed by `/` and each listed suffix; the syntax row is the full cross-product, with no implied intermediate parent tests.

| Parent test | Passing suffixes | Subtests |
| --- | --- | ---: |
| `TestUsageQueryWindowBoundaries` | `hour`, `day` | 2 |
| `TestImportedUsagePreservesHistoricalDimensionSyntax` | Each of `usage_events`, `usage_rollups`, followed by `/`, then each of `legacy_custom`, `single_letter`, `maximum_length`, `digits_and_underscores`, `too_long`, `uppercase`, `leading_digit`, `hyphen`, `newline`, `non_ascii`, `empty`, `null`, `number` | 26 |
| `TestUsagePostgresRoundTrip` | `hostile_import_usage_events`, `hostile_import_usage_rollups`, `hostile_import_high_cardinality` | 3 |
| `TestImportUsageLegacyDimensionRoundTripPostgres` | `malformed_usage_events`, `malformed_usage_rollups` | 2 |
| `TestUsageTruncationContract` | `complete`, `truncated` | 2 |
| `TestUsageRequiresTruncationOptIn` | `exact_cap`, `old_client_cap_plus_one`, `explicit_opt_out`, `opt_in`, `unnegotiated_callback_partial`, `invalid_opt_in`, `ambiguous_opt_in` | 7 |
| `TestUsageCommandFailsWithoutTruncationOptIn` | `default`, `json`, `explicit_false` | 3 |

| Package | Named passes, including parents | Observed shuffle seed |
| --- | ---: | --- |
| `internal/store` | 42 | `1788794977569158000` |
| `internal/server` | 12 | `1788795050707186000` |
| `internal/client` | 4 | `1788795051062894000` |
| `cmd/witself` | 5 | `1788795065529253000` |
| `cmd/witself-server` | 1 | `1788795065027610000` |

Final value-free queries found zero fixture accounts and zero migration scratch schemas. The owned database was dropped and its absence was checked by both the runner and a separate root verification. Shared-lock and recorded process-group absence were independently observed; temporary-state removal was recorded by the runner. All 1,644 tracked source files remained unchanged during verification, and pre-existing checkout work was preserved.

The following SHA-256 digests identify the complete retained private evidence. Raw streams remain private; these identifiers provide provenance without publishing arbitrary test output, account/archive content, local paths or connection details.

| Record | SHA-256 |
| --- | --- |
| Frozen source baseline, all 1,644 tracked files | `5b8498e2b5bae396aa73cbb1cdda60a5db8e6cc52b05f9303cf0bac7e4f97650` |
| Complete source snapshot before and after, identical | `e634723b80bc218a46c42c228cfee1261d48aef3810f6f016e22c3ff4b46c280` |
| Toolchain identity baseline | `11b2c91f23a7f59c5751b1a9e394e999955aab85f3bcbc03c05a13ecf0e662b9` |
| Go binary | `a1c83801d1756c3eca78366c6b585f2c21c20694fb1c7eb92c446a0580420412` |
| Node 22.23.2 binary, identity check only | `18e387c90ab8a8400183e8bdd396376e1e875b91b4c874b894dcade7b35bf572` |
| Reviewed external focused runner | `0a25bfc65f1800208f05cc4ebb29880f39059da373726f95a00a62a59f4b8ee6` |
| Complete store JSON stream, 61,898 bytes | `29ae697876c470443a2a006ee31e73d5c73d4504e2b34189e1acfd79fb14eeda` |
| Complete boundary JSON stream, 20,717 bytes | `ba4332a9afa34e3e5e6521f062daa9d42929bc39f5c84ab8e76e27aa60ffed3f` |
| Both test stderr streams, empty | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |
| Exact named outcome inventory | `06d8f0dbcc8867495e430c0437b9362c8bbd362296d2fc7d74ab037cc15a3273` |
| Complete focused result and command/log manifest | `491da14f2f3b2bc0538445ae6f7c034e2ae69848548d16b023656d292c3d902b` |
| Applied-schema output | `d9323adde4f1cb512e16a8335f30a318b830c39e8d4e61e7780eabda7cafea82` |
| Actual outer exit and independent database-absence receipt | `26babe7d9e7953315aa6001bdd4668d139ffdb4efee29bc62a30b32ab4240022` |
| Independent complete focused evidence review | `ea77ed1217c85a3ffc835fb9dc25102e4e17ae8fcf006fc532a66daea9fd7909` |

This evidence supports the usage-query-bounds catalog gate. A returned-point cap does **not** establish SQL scan/sort cost, latency, throughput, general memory bounds or production-shaped load. It does not certify billing/account aggregation, live managed cells, continuous monitoring or general rollout readiness. Managed rollout remains limited and overall usage readiness conditional; the separate observability and usage-rollup expansion/design holds remain open.
