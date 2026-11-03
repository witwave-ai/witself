package store

import (
	"bytes" // LC-S6C1 fix 3 (G1)
	"context"
	"errors"
	"net" // LC-S6C1 fix 3 (G1)
	"regexp"
	"strings"
	"sync"        // LC-S6C1 fix 3 (G1)
	"sync/atomic" // LC-S6C1 fix 3 (G1)
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool" // LC-S6C1 fix 3 (G1)
	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

// Optional scope distinguishes the stale transaction deliberately held by a
// test from an unrelated backend. It also applies the production horizon
// predicate without logging account, backend or transaction identifiers.
type syncCaptureActivityScope struct {
	account     string
	expectedPID uint32
}

// Diagnostics show bounded metadata and sanitized SQL structure only. Redaction
// runs before truncation so the boundary cannot disclose part of a literal,
// connection string, credential assignment, or long opaque token.
func logSyncCaptureActivityTypes(t *testing.T, st *Store, scopes ...syncCaptureActivityScope) {
	t.Helper()
	var scope syncCaptureActivityScope
	if len(scopes) != 0 {
		scope = scopes[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := st.pool.Query(ctx, `SELECT
		coalesce(application_name,''),
		CASE backend_type WHEN 'client backend' THEN 'client backend'
		WHEN 'autovacuum worker' THEN 'autovacuum worker' WHEN 'autovacuum launcher' THEN 'autovacuum launcher'
		WHEN 'parallel worker' THEN 'parallel worker' ELSE 'other' END,
		CASE state WHEN 'active' THEN 'active' WHEN 'idle' THEN 'idle'
		WHEN 'idle in transaction' THEN 'idle in transaction'
		WHEN 'idle in transaction (aborted)' THEN 'idle in transaction (aborted)' ELSE 'other' END,
		CASE WHEN xact_start IS NULL THEN 'none'
		WHEN clock_timestamp()-xact_start < interval '1 second' THEN 'under_1s'
		WHEN clock_timestamp()-xact_start < interval '10 seconds' THEN '1s_to_10s'
		WHEN clock_timestamp()-xact_start < interval '1 minute' THEN '10s_to_1m'
		ELSE 'at_least_1m' END,
		coalesce(query,''), pid=$2,
		CASE WHEN $1='' THEN 'unscoped'
		WHEN NOT EXISTS(SELECT 1 FROM accounts WHERE id=$1) THEN 'account_absent'
		WHEN EXISTS(SELECT 1 FROM accounts account WHERE account.id=$1
		AND ((activity.backend_xmin IS NOT NULL AND age(activity.backend_xmin)>=age(account.xmin)
		AND (activity.backend_type IS NULL OR activity.backend_type NOT IN ('autovacuum worker','autovacuum launcher')))
		OR (activity.usesysid IS DISTINCT FROM (SELECT oid FROM pg_roles WHERE rolname=current_user)
		AND NOT (current_setting('is_superuser')='on' OR pg_has_role(current_user,'pg_read_all_stats','MEMBER')))))
		THEN 'matches' ELSE 'clear' END
		FROM pg_stat_activity activity WHERE datname=current_database() AND pid<>pg_backend_pid()
		ORDER BY backend_type,state,application_name,pid`, scope.account, int64(scope.expectedPID))
	if err != nil {
		t.Log("capture activity diagnostics unavailable")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var application, backend, state, age, query, horizon string
		var expected bool
		if rows.Scan(&application, &backend, &state, &age, &query, &expected, &horizon) != nil {
			t.Log("capture activity diagnostics unavailable")
			return
		}
		t.Logf("capture activity application_name=%q backend=%s state=%s xact_age=%s horizon=%s expected_test_transaction=%t query_prefix=%q",
			syncCaptureDiagnosticText(application, 63), backend, state, age, horizon, expected, syncCaptureDiagnosticText(query, 80))
	}
	if rows.Err() != nil {
		t.Log("capture activity diagnostics unavailable")
	}
}

var syncCaptureDiagnosticRedactions = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:[a-z][a-z0-9+.-]*://)[^\s]+`),
	regexp.MustCompile(`(?i)\b(?:password|passwd|token|secret|credential|authorization|api[_-]?key)\s*[:=]\s*[^\s,;]+`),
	regexp.MustCompile(`(?i)\bbearer\s+[^\s,;]+`),
	regexp.MustCompile(`[[:xdigit:]]{32,}`),
	regexp.MustCompile(`[A-Za-z0-9_+/=-]{48,}`),
}

// Scan structural SQL forms before generic credential redaction. PostgreSQL
// block comments nest and dollar delimiters must match exactly. Incomplete
// quoted/comment forms consume the remaining input because activity text may
// have been truncated by the server in the middle of a sensitive value.
func syncCaptureDiagnosticSQL(text string) string {
	var result strings.Builder
	for offset := 0; offset < len(text); {
		rest := text[offset:]
		switch {
		case strings.HasPrefix(rest, "--"):
			end := strings.IndexAny(rest, "\r\n")
			if end < 0 {
				end = len(rest)
			}
			offset += end
			result.WriteString("[redacted]")
		case strings.HasPrefix(rest, "/*"):
			end, depth := 2, 1
			for end < len(rest) && depth != 0 {
				switch {
				case strings.HasPrefix(rest[end:], "/*"):
					depth++
					end += 2
				case strings.HasPrefix(rest[end:], "*/"):
					depth--
					end += 2
				default:
					end++
				}
			}
			offset += end
			result.WriteString("[redacted]")
		case rest[0] == '\'' || rest[0] == '"':
			quote, end := rest[0], 1
			for end < len(rest) {
				if rest[end] == '\\' {
					// Without the session's string-conformance setting a
					// backslash makes later quote boundaries ambiguous. Hide
					// the remainder instead of exposing a subsequent literal.
					end = len(rest)
					break
				}
				if rest[end] == quote {
					end++
					if end < len(rest) && rest[end] == quote {
						end++
						continue
					}
					break
				}
				end++
			}
			offset += end
			result.WriteString("[redacted]")
		case rest[0] == '$':
			delimiterEnd := 1
			for delimiterEnd < len(rest) && syncCaptureDiagnosticTagByte(rest[delimiterEnd]) {
				delimiterEnd++
			}
			if delimiterEnd >= len(rest) || rest[delimiterEnd] != '$' {
				result.WriteByte(rest[0])
				offset++
				continue
			}
			delimiterEnd++
			delimiter := rest[:delimiterEnd]
			end := strings.Index(rest[delimiterEnd:], delimiter)
			if end < 0 {
				offset = len(text)
			} else {
				offset += delimiterEnd + end + len(delimiter)
			}
			result.WriteString("[redacted]")
		default:
			result.WriteByte(rest[0])
			offset++
		}
	}
	return result.String()
}

func syncCaptureDiagnosticTagByte(character byte) bool {
	// High bytes preserve non-ASCII PostgreSQL identifier tags. Accepting an
	// initial digit conservatively redacts a few invalid SQL forms as well.
	return character >= 0x80 || character == '_' || character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}

func syncCaptureDiagnosticText(text string, limit int) string {
	text = syncCaptureDiagnosticSQL(text)
	for _, pattern := range syncCaptureDiagnosticRedactions {
		text = pattern.ReplaceAllString(text, "[redacted]")
	}
	text = strings.Join(strings.Fields(text), " ")
	characters := []rune(text)
	if len(characters) > limit {
		text = string(characters[:limit])
	}
	return text
}

func TestSyncCaptureRefusesSnapshotsBeforeAccount(t *testing.T) {
	for _, isolation := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		for _, entrypoint := range []string{"enable", "activate"} {
			t.Run(string(isolation)+"/"+entrypoint, func(t *testing.T) {
				setupStarted := time.Now() // LC-S6C1 fix 3 (G1)
				st, _ := newSyncSchemaTestStore(t)
				setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer setupCancel()
				if st.MigrateSync(setupCtx) != nil || st.SelectSyncCapture(runtimeconfig.Config{Local: true, SyncSchema: true}) != nil {
					t.Fatal("capture preaccount fixture selection failed")
				}
				setupCancel()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				old, err := st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: isolation})
				if err != nil {
					t.Fatal("capture preaccount old transaction failed")
				}
				defer func() { _ = old.Rollback(ctx) }()
				const account = "late_capture_account"
				var visible bool
				if old.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, account).Scan(&visible) != nil || visible {
					t.Fatal("capture preaccount snapshot setup failed")
				}
				for _, sql := range []string{
					`INSERT INTO accounts(id,display_name,is_default,status) VALUES('late_capture_account','Late capture fixture',false,'active')`,
					`INSERT INTO sync_incarnations(account_id,lifecycle_epoch,restore_marker,allocation_id,transition_id,authority_installation,state)
					VALUES('late_capture_account',1,decode(repeat('01',16),'hex'),decode(repeat('02',16),'hex'),decode(repeat('03',16),'hex'),'local','installed')`,
				} {
					if _, err := st.pool.Exec(ctx, sql); err != nil {
						t.Fatal("capture preaccount new account setup failed")
					}
				}
				if entrypoint == "enable" {
					if err := st.EnableSyncWriter(ctx, account, SyncPreferencesPrincipal, SyncPreferencesCapability); !errors.Is(err, ErrSyncWriterNotAdmitted) {
						t.Fatal("capture preaccount capability enabled over old snapshot")
					}
					var count int
					if st.pool.QueryRow(ctx, `SELECT count(*) FROM sync_writer_capabilities WHERE account_id=$1`, account).Scan(&count) != nil || count != 0 {
						t.Fatal("capture preaccount refused enable left capability state")
					}
				} else {
					// Raw fixture seeding isolates activation's own check from the
					// earlier Enable check. No principal or role is created.
					if _, err := st.pool.Exec(ctx, `INSERT INTO sync_writer_capabilities(account_id,principal,capability,manifest_version,enabled,capture_epoch)
						VALUES($1,$2,$3,$4,true,1)`, account, SyncPreferencesPrincipal, SyncPreferencesCapability, SyncCoverageManifestVersion); err != nil {
						t.Fatal("capture preaccount activation capability setup failed")
					}
					owner, err := st.ActivateSyncCapture(ctx, account, SyncCaptureOptions{
						LifecycleEpoch: 1, RestoreMarker: []byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, Capability: SyncPreferencesCapability,
					})
					if owner != nil {
						owner.Close()
					}
					if !errors.Is(err, ErrSyncWriterNotAdmitted) || owner != nil {
						t.Fatal("capture preaccount activation accepted old snapshot")
					}
					var count int
					if st.pool.QueryRow(ctx, `SELECT count(*) FROM sync_capture WHERE account_id=$1`, account).Scan(&count) != nil || count != 0 {
						t.Fatal("capture preaccount refused activation left control state")
					}
				}
				if old.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, account).Scan(&visible) != nil || visible {
					t.Fatal("capture preaccount old snapshot unexpectedly advanced")
				}
				if old.Rollback(ctx) != nil {
					t.Fatal("capture preaccount old snapshot release failed")
				}
				// Minimum 5s, multiplier 10 of this subcase's migration, snapshot and refusal setup. // LC-S6C1 fix 3 (G1)
				// A separate context keeps the earlier 20s context from clipping the resume allowance. // LC-S6C1 fix 3 (G1)
				resumeAllowance := max(5*time.Second, 10*time.Since(setupStarted))                    // LC-S6C1 fix 3 (G1)
				resumeCtx, resumeCancel := context.WithTimeout(context.Background(), resumeAllowance) // LC-S6C1 fix 3 (G1)
				defer resumeCancel()                                                                  // LC-S6C1 fix 3 (G1)
				ticker := time.NewTicker(10 * time.Millisecond)                                       // LC-S6C1 fix 3 (G1)
				defer ticker.Stop()                                                                   // LC-S6C1 fix 3 (G1)
				refusedAttempts := 0                                                                  // LC-S6C1 fix 3 (G1)
				defer func() { t.Logf("capture resume refused attempts %d", refusedAttempts) }()      // LC-S6C1 fix 3 (G1)
				for {                                                                                 // LC-S6C1 fix 3 (G1)
					// Each attempt has its own 20s operation bound; the resume allowance bounds only retry waits. // LC-S6C1 fix 4 (G1)
					enableCtx, enableCancel := context.WithTimeout(context.Background(), 20*time.Second)                // LC-S6C1 fix 4 (G1)
					err := st.EnableSyncWriter(enableCtx, account, SyncPreferencesPrincipal, SyncPreferencesCapability) // LC-S6C1 fix 3 (G1) // LC-S6C1 fix 4 (G1)
					attemptErr := enableCtx.Err()                                                                       // LC-S6C1 fix 4 (G1)
					enableCancel()                                                                                      // LC-S6C1 fix 4 (G1)
					if err == nil {                                                                                     // LC-S6C1 fix 3 (G1)
						break // LC-S6C1 fix 3 (G1)
					} // LC-S6C1 fix 3 (G1)
					if !errors.Is(err, ErrSyncWriterNotAdmitted) { // LC-S6C1 fix 3 (G1)
						t.Fatal("G1othererror resume returned a non sentinel error") // LC-S6C1 fix 3 (G1)
					} // LC-S6C1 fix 3 (G1)
					if attemptErr != nil { // LC-S6C1 fix 4 (G1)
						refusedAttempts++                                                 // LC-S6C1 fix 4 (G1)
						t.Fatal("G1attemptctx attempt context expired before resumption") // LC-S6C1 fix 4 (G1)
					} // LC-S6C1 fix 4 (G1)
					refusedAttempts++ // LC-S6C1 fix 3 (G1)
					select {          // LC-S6C1 fix 3 (G1)
					case <-resumeCtx.Done(): // LC-S6C1 fix 3 (G1)
					case <-ticker.C: // LC-S6C1 fix 3 (G1)
					} // LC-S6C1 fix 3 (G1)
					if resumeCtx.Err() != nil { // LC-S6C1 fix 3 (G1)
						// The sentinel also covers non-horizon failures. Prove an older xmin before naming it. // LC-S6C1 fix 3 (G1)
						// This fresh autocommit query uses the production horizon predicate and exposes only a count. // LC-S6C1 fix 3 (G1)
						observeCtx, observeCancel := context.WithTimeout(context.Background(), 5*time.Second)                                     // LC-S6C1 fix 3 (G1)
						defer observeCancel()                                                                                                     // LC-S6C1 fix 3 (G1)
						var blockers int                                                                                                          // LC-S6C1 fix 3 (G1)
						if st.pool.QueryRow(observeCtx, "SELECT count(*) FROM pg_stat_activity activity JOIN accounts account ON account.id=$1 "+ // LC-S6C1 fix 3 (G1)
							"WHERE activity.datname=current_database() AND activity.pid<>pg_backend_pid() "+ // LC-S6C1 fix 3 (G1)
							"AND activity.backend_xmin IS NOT NULL AND age(activity.backend_xmin)>=age(account.xmin) "+ // LC-S6C1 fix 3 (G1)
							"AND (activity.backend_type IS NULL OR activity.backend_type NOT IN ('autovacuum worker','autovacuum launcher'))", account).Scan(&blockers) != nil { // LC-S6C1 fix 3 (G1)
							t.Fatal("G1countquery horizon observation failed") // LC-S6C1 fix 3 (G1)
						} // LC-S6C1 fix 3 (G1)
						if blockers > 0 { // LC-S6C1 fix 3 (G1)
							t.Fatalf("G1horizonold resume still refused with older xmin backend count %d", blockers) // LC-S6C1 fix 3 (G1)
						} // LC-S6C1 fix 3 (G1)
						t.Fatal("G1unprovenxx resume sentinel without an observed older xmin") // LC-S6C1 fix 3 (G1)
					} // LC-S6C1 fix 3 (G1)
				} // LC-S6C1 fix 3 (G1)
				activateSyncFixture(t, st, account)
				t.Log("capture observed preaccount snapshot refusal and successful enable/activation after snapshot release")
			})
		}
	}
} // LC-S6C1 fix 3 (G1)
// LC-S6C1 fix 3 (G1)
// syncCaptureDelayedTerminateConn holds only transport teardown so SQL cleanup must stand on its own. // LC-S6C1 fix 3 (G1)
type syncCaptureDelayedTerminateConn struct { // LC-S6C1 fix 3 (G1)
	net.Conn                  // LC-S6C1 fix 3 (G1)
	terminateSeen atomic.Bool // LC-S6C1 fix 3 (G1)
	closeSeen     atomic.Bool // LC-S6C1 fix 3 (G1)
} // LC-S6C1 fix 3 (G1)
// LC-S6C1 fix 3 (G1)
func (c *syncCaptureDelayedTerminateConn) Write(data []byte) (int, error) { // LC-S6C1 fix 3 (G1)
	if bytes.Equal(data, []byte{'X', 0, 0, 0, 4}) { // LC-S6C1 fix 3 (G1)
		c.terminateSeen.Store(true) // LC-S6C1 fix 3 (G1)
		return len(data), nil       // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	return c.Conn.Write(data) // LC-S6C1 fix 3 (G1)
} // LC-S6C1 fix 3 (G1)
// LC-S6C1 fix 3 (G1)
func (c *syncCaptureDelayedTerminateConn) Close() error { // LC-S6C1 fix 3 (G1)
	c.closeSeen.Store(true) // LC-S6C1 fix 3 (G1)
	return nil              // LC-S6C1 fix 3 (G1)
} // LC-S6C1 fix 3 (G1)
// LC-S6C1 fix 3 (G1)
func TestSyncCaptureRefusalReleasesSessionGateBeforeTransportClose(t *testing.T) { // LC-S6C1 fix 3 (G1)
	st, _ := newSyncSchemaTestStore(t)                                                // LC-S6C1 fix 3 (G1)
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute) // LC-S6C1 fix 3 (G1)
	defer setupCancel()                                                               // LC-S6C1 fix 3 (G1)
	if st.MigrateSync(setupCtx) != nil {                                              // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup01 fixture migration failed") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)      // LC-S6C1 fix 3 (G1)
	defer cancel()                                                                // LC-S6C1 fix 3 (G1)
	old, err := st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}) // LC-S6C1 fix 3 (G1)
	if err != nil {                                                               // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup02 old snapshot begin failed") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	defer func() { _ = old.Rollback(context.Background()) }()                                                             // LC-S6C1 fix 3 (G1)
	const account = "cleanup_capture_account"                                                                             // LC-S6C1 fix 3 (G1)
	var visible bool                                                                                                      // LC-S6C1 fix 3 (G1)
	if old.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, account).Scan(&visible) != nil || visible { // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup03 old snapshot pin failed") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	if _, err := st.pool.Exec(ctx, `INSERT INTO accounts(id,display_name,is_default,status) VALUES($1,'Cleanup capture fixture',false,'active')`, account); err != nil { // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup04 account fixture failed") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	config := st.pool.Config()                                                                          // LC-S6C1 fix 3 (G1)
	config.MaxConns = 1                                                                                 // LC-S6C1 fix 3 (G1)
	config.MinConns = 0                                                                                 // LC-S6C1 fix 3 (G1)
	const application = "lc_s6c1_fix3_cleanup"                                                          // LC-S6C1 fix 3 (G1)
	config.ConnConfig.RuntimeParams["application_name"] = application                                   // LC-S6C1 fix 3 (G1)
	dial := config.ConnConfig.DialFunc                                                                  // LC-S6C1 fix 3 (G1)
	var wrappersMu sync.Mutex                                                                           // LC-S6C1 fix 3 (G1)
	var wrappers []*syncCaptureDelayedTerminateConn                                                     // LC-S6C1 fix 3 (G1)
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) { // LC-S6C1 fix 3 (G1)
		conn, err := dial(ctx, network, address) // LC-S6C1 fix 3 (G1)
		if err != nil {                          // LC-S6C1 fix 3 (G1)
			return nil, err // LC-S6C1 fix 3 (G1)
		} // LC-S6C1 fix 3 (G1)
		wrapped := &syncCaptureDelayedTerminateConn{Conn: conn} // LC-S6C1 fix 3 (G1)
		wrappersMu.Lock()                                       // LC-S6C1 fix 3 (G1)
		wrappers = append(wrappers, wrapped)                    // LC-S6C1 fix 3 (G1)
		wrappersMu.Unlock()                                     // LC-S6C1 fix 3 (G1)
		return wrapped, nil                                     // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	pool, err := pgxpool.NewWithConfig(ctx, config) // LC-S6C1 fix 3 (G1)
	if err != nil {                                 // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup05 dedicated pool failed") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	t.Cleanup(func() { // LC-S6C1 fix 3 (G1)
		wrappersMu.Lock()                  // LC-S6C1 fix 3 (G1)
		for _, wrapped := range wrappers { // LC-S6C1 fix 3 (G1)
			_ = wrapped.Conn.Close() // LC-S6C1 fix 3 (G1)
		} // LC-S6C1 fix 3 (G1)
		wrappersMu.Unlock() // LC-S6C1 fix 3 (G1)
		pool.Close()        // LC-S6C1 fix 3 (G1)
	}) // LC-S6C1 fix 3 (G1)
	activationStore := &Store{pool: pool, syncPrincipalTest: st.syncPrincipalTest}                     // LC-S6C1 fix 3 (G1)
	if activationStore.SelectSyncCapture(runtimeconfig.Config{Local: true, SyncSchema: true}) != nil { // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup06 capture selection failed") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	conn, err := pool.Acquire(ctx) // LC-S6C1 fix 3 (G1)
	if err != nil {                // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup07 gate connection failed") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	pid := conn.Conn().PgConn().PID()                                                   // LC-S6C1 fix 3 (G1)
	conn.Release()                                                                      // LC-S6C1 fix 3 (G1)
	owner, err := activationStore.ActivateSyncCapture(ctx, account, SyncCaptureOptions{ // LC-S6C1 fix 3 (G1)
		LifecycleEpoch: 1, RestoreMarker: bytes.Repeat([]byte{1}, 16), Capability: SyncPreferencesCapability, // LC-S6C1 fix 3 (G1)
	}) // LC-S6C1 fix 3 (G1)
	if owner != nil { // LC-S6C1 fix 3 (G1)
		owner.Close() // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	if !errors.Is(err, ErrSyncWriterNotAdmitted) || owner != nil { // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup08 old snapshot was not refused") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	wrappersMu.Lock()                                                                                      // LC-S6C1 fix 3 (G1)
	teardownHeld := len(wrappers) == 1 && wrappers[0].terminateSeen.Load() && wrappers[0].closeSeen.Load() // LC-S6C1 fix 3 (G1)
	wrappersMu.Unlock()                                                                                    // LC-S6C1 fix 3 (G1)
	if !teardownHeld {                                                                                     // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup09 transport teardown seam was not reached") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	observer, err := pgx.ConnectConfig(ctx, st.pool.Config().ConnConfig.Copy()) // LC-S6C1 fix 3 (G1)
	if err != nil {                                                             // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup10 observer connection failed") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	defer func() { _ = observer.Close(context.Background()) }() // LC-S6C1 fix 3 (G1)
	// The sole dedicated session is identified by its captured PID and application name, never by query text. // LC-S6C1 fix 3 (G1)
	var sessions, openTransactions int                                                                                  // LC-S6C1 fix 3 (G1)
	if observer.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE xact_start IS NOT NULL) FROM pg_stat_activity `+ // LC-S6C1 fix 3 (G1)
		`WHERE datname=current_database() AND pid<>pg_backend_pid() AND pid=$1 AND application_name=$2`, // LC-S6C1 fix 3 (G1)
		int64(pid), application).Scan(&sessions, &openTransactions) != nil { // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup11 activity observation failed") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	if sessions != 1 { // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup12 delayed session was not observed") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	if openTransactions != 0 { // LC-S6C1 fix 3 (G1)
		t.Fatalf("G1cleanup13 refused activation retained open transaction count=%d", openTransactions) // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	var locked bool                                                                                                           // LC-S6C1 fix 3 (G1)
	if observer.QueryRow(ctx, `SELECT pg_try_advisory_lock(capture_lock_key($1))`, account).Scan(&locked) != nil || !locked { // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup14 refused activation retained session gate") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
	var unlocked bool                                                                                                           // LC-S6C1 fix 3 (G1)
	if observer.QueryRow(ctx, `SELECT pg_advisory_unlock(capture_lock_key($1))`, account).Scan(&unlocked) != nil || !unlocked { // LC-S6C1 fix 3 (G1)
		t.Fatal("G1cleanup15 observer gate release failed") // LC-S6C1 fix 3 (G1)
	} // LC-S6C1 fix 3 (G1)
} // LC-S6C1 fix 3 (G1)
