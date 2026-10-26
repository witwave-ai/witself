// Package syncharness supplies synthetic scenarios, a deterministic model clock,
// fault selection, and replayable schedules for Local Cell synchronization tests.
//
// This is scaffolding, not synchronization. It installs no transport adapter,
// production writer, endpoint, worker, migration, identity, or recovery protocol.
// A model pass is not PostgreSQL evidence or an M4 synchronization result. The
// model clock never overrides production SQL time. Scheduler, clock, and random
// streams have one owner; callers must not use them concurrently. Trace data must
// contain only public synthetic symbols, never payloads, credentials, or DSNs.
//
// PostgreSQL fixtures use WITSELF_TEST_DATABASE_URL, like other integration
// tests. Its host must be 127.0.0.1, localhost, or ::1; only the sslmode query
// option is allowed. There is no database-name allowlist. WITSELF_SYNC_HARNESS_DB
// is an optional lane guard: when set, it must match the URL's database name.
// An unset URL skips with NOT RUN; WITSELF_TEST_REQUIRE_DATABASE=1 makes it fail.
// Fixtures never select another database or fall back to an unconfigured URL.
package syncharness
