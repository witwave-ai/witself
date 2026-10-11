package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/routeinventory"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/testenv"
)

var tenantMatrixSchemaSequence atomic.Uint64

// Empty deliberately: exclusions need a citation to an observed writer, not an
// assumption that authentication or background work might change a table.
var tenantFingerprintVolatile = map[string]string{}

// tenantNewRuntime follows startupCohortStore's private-schema lifecycle. Its
// schema DSN is used by both store connections and the child, never the base DSN.
func tenantNewRuntime(t *testing.T) *tenantRuntime {
	t.Helper()
	dsn := testenv.RequirePostgres(t)
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") ||
		(u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.Path == "" || u.Path == "/" {
		t.Fatal("tenant matrix requires an explicitly configured loopback test database")
	}
	// The worker runs only codex_a:5599. CI supplies its own test database
	// through this existing testenv contract; neither path opens another DB.
	// Connection overrides could silently escape the selected database or host.
	for name := range u.Query() {
		if name != "sslmode" {
			t.Fatal("tenant matrix database URL permits only the sslmode parameter")
		}
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("tenant matrix database connection setup failed")
	}
	rt := &tenantRuntime{
		schema:       fmt.Sprintf("witself_tenant_matrix_%d_%d", os.Getpid(), tenantMatrixSchemaSequence.Add(1)),
		f:            syntheticTenantFixture(),
		creds:        map[tenantSide]map[tenantCred]string{},
		freshCreds:   map[tenantSide]map[string]string{},
		scratchCreds: map[string]string{},
		urls:         map[routeinventory.Listener]string{},
		client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
	rt.f.bySide[sideA] = tenantSyntheticValues(sideA, "runtime")
	for _, side := range []tenantSide{sideA, sideB} {
		rt.creds[side] = map[tenantCred]string{}
		rt.freshCreds[side] = map[string]string{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+tenantQuoteIdentifier(rt.schema)); err != nil {
		_ = admin.Close()
		t.Fatalf("tenant matrix schema creation failed: %s", tenantHarnessSafeError(err))
	}
	// Registered before child shutdown, so Cleanup's LIFO order stops all child
	// connections before dropping the schema.
	t.Cleanup(func() {
		rt.client.CloseIdleConnections()
		if rt.st != nil {
			rt.st.Close()
		}
		if rt.db != nil {
			_ = rt.db.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := admin.ExecContext(ctx, `DROP SCHEMA `+tenantQuoteIdentifier(rt.schema)+` CASCADE`); err != nil {
			t.Errorf("tenant matrix schema cleanup failed: %s", tenantHarnessSafeError(err))
		}
		if err := admin.Close(); err != nil {
			t.Error("tenant matrix admin connection close failed")
		}
	})
	q := u.Query()
	q.Set("search_path", rt.schema)
	u.RawQuery = q.Encode()
	rt.dsn = u.String()
	rt.st, err = store.Open(context.Background(), rt.dsn)
	if err != nil {
		t.Fatalf("tenant matrix store open failed: %s", tenantHarnessSafeError(err))
	}
	if err := rt.st.Migrate(); err != nil {
		t.Fatalf("tenant matrix schema migration failed: %s", tenantHarnessSafeError(err))
	}
	rt.db, err = sql.Open("pgx", rt.dsn)
	if err != nil {
		t.Fatal("tenant matrix schema connection setup failed")
	}
	return rt
}

func tenantQuoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func tenantHarnessRandom(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal("tenant matrix entropy source failed")
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// Only preserve known infrastructure error reasons, never database error text
// which may contain a row, credential, or archive field value.
func tenantHarnessSafeError(err error) string {
	for _, reason := range []string{"operation not permitted", "permission denied", "connection refused", "address already in use", "context deadline exceeded"} {
		if strings.Contains(strings.ToLower(err.Error()), reason) {
			return reason
		}
	}
	return fmt.Sprintf("%T (details suppressed)", err)
}

func tenantStartChild(t *testing.T, rt *tenantRuntime) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("tenant matrix could not locate its test binary")
	}
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("tenant matrix could not generate its test relay key")
	}
	// Same valid relay-key shape as setAgentEmailRelayPublicKeyEnv.
	publicKeys, _ := json.Marshal(map[string]string{"tenant-matrix": base64.StdEncoding.EncodeToString(publicKey)})
	provision := tenantHarnessRandom(t)
	backup := tenantHarnessRandom(t)
	for backup == provision {
		backup = tenantHarnessRandom(t)
	}
	bootstrap, provider := tenantHarnessRandom(t), tenantHarnessRandom(t)
	rt.secrets = append(rt.secrets, provision, backup, bootstrap, provider)
	for _, side := range []tenantSide{sideA, sideB} {
		rt.creds[side][credProvision] = provision
		rt.creds[side][credBackup] = backup
	}
	// parseAgentEmailProductionAccountIDs requires lexically sorted IDs; A/B
	// role order has no relationship to randomly generated account IDs.
	receiveAccounts := []string{rt.f.bySide[sideA]["account"], rt.f.bySide[sideB]["account"]}
	sort.Strings(receiveAccounts)
	// Explicit allowlist. In particular no account request budget, token-file,
	// proxy, inherited database, or home setting can reach the child.
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"WITSELF_TEST_TENANT_MATRIX_CHILD=1",
		"WITSELF_DATABASE_URL=" + rt.dsn,
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		"WITSELF_HOME=" + t.TempDir(),
		"DSH_HOME=" + t.TempDir(),
		"WITSELF_PROVISION_TOKEN=" + provision,
		"WITSELF_BACKUP_TOKEN=" + backup,
		"WITSELF_BOOTSTRAP_TOKEN=" + bootstrap,
		"WITSELF_FACT_DELETION_ENABLED=true",
		"WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED=true",
		"WITSELF_AGENT_EMAIL_RECEIVE_DOMAIN=witmail.net",
		"WITSELF_AGENT_EMAIL_RECEIVE_AUDIENCE=tenant-matrix",
		"WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS=" + strings.Join(receiveAccounts, ","),
		"WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON=" + string(publicKeys),
		"WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID=" + rt.f.bySide[sideB]["agent"],
		"WITSELF_AGENT_EMAIL_PROVIDER_EVENT_TOKEN=" + provider,
		"WITSELF_SUPPORT_TICKET_RATE_LIMIT=1000",
	}
	if value, ok := os.LookupEnv("GORACE"); ok {
		env = append(env, "GORACE="+value)
	}
	for attempt := 0; attempt < 3; attempt++ {
		addresses := tenantLoopbackAddresses(t)
		childEnv := append([]string(nil), env...)
		for _, pair := range []struct {
			listener routeinventory.Listener
			variable string
		}{{routeinventory.ListenerAPI, "WITSELF_API_ADDR"}, {routeinventory.ListenerHealth, "WITSELF_HEALTH_ADDR"}, {routeinventory.ListenerMetrics, "WITSELF_METRICS_ADDR"}} {
			childEnv = append(childEnv, pair.variable+"="+addresses[pair.listener])
			rt.urls[pair.listener] = "http://" + addresses[pair.listener]
		}
		stderr := &tenantChildStderr{}
		cmd := exec.Command(executable)
		cmd.Env = childEnv
		cmd.Stderr = stderr
		cmd.Stdout = io.Discard
		if err := cmd.Start(); err != nil {
			t.Fatalf("tenant matrix child exec failed: %s", tenantHarnessSafeError(err))
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		finished, retry := false, false
		t.Cleanup(func() {
			if finished {
				return
			}
			select {
			case err := <-done:
				finished = true
				if err != nil {
					t.Errorf("tenant matrix child exited unsuccessfully: %s", tenantHarnessSafeError(err))
				}
			default:
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Errorf("tenant matrix child SIGTERM failed: %s", tenantHarnessSafeError(err))
				}
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("tenant matrix child must exit zero: %s", tenantHarnessSafeError(err))
					}
				case <-time.After(15 * time.Second):
					if err := cmd.Process.Kill(); err != nil {
						t.Errorf("tenant matrix child kill failed: %s", tenantHarnessSafeError(err))
					} else {
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							t.Error("tenant matrix child did not exit after kill")
						}
					}
					t.Error("tenant matrix child did not shut down after SIGTERM")
				}
				finished = true
			}
			if t.Failed() {
				t.Logf("tenant matrix child stderr (last 200 lines):\n%s", tenantRedactChildLog(stderr.String(), rt))
			}
		})
		readyCtx, readyCancel := context.WithTimeout(context.Background(), 60*time.Second)
		poll := time.NewTicker(100 * time.Millisecond)
		for {
			select {
			case err := <-done:
				finished = true
				if strings.Contains(stderr.String(), "address already in use") && attempt < 2 {
					retry = true
					break
				}
				readyCancel()
				poll.Stop()
				t.Logf("tenant matrix child stderr (last 200 lines):\n%s", tenantRedactChildLog(stderr.String(), rt))
				if err == nil {
					t.Fatal("tenant matrix child exited before readiness")
				}
				t.Fatalf("tenant matrix child failed before readiness: %s", tenantHarnessSafeError(err))
			case <-readyCtx.Done():
				readyCancel()
				poll.Stop()
				t.Fatal("tenant matrix child readiness timed out")
			case <-poll.C:
				req, err := http.NewRequestWithContext(readyCtx, http.MethodGet, rt.urls[routeinventory.ListenerHealth]+"/readyz", nil)
				if err != nil {
					readyCancel()
					poll.Stop()
					t.Fatal("tenant matrix readiness request construction failed")
				}
				resp, err := rt.client.Do(req)
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						readyCancel()
						poll.Stop()
						return
					}
				}
			}
			if retry {
				readyCancel()
				poll.Stop()
				break
			}
		}
	}
	t.Fatal("tenant matrix child exhausted bind retries")
}

func tenantLoopbackAddresses(t *testing.T) map[routeinventory.Listener]string {
	t.Helper()
	addresses := map[routeinventory.Listener]string{}
	var listeners []net.Listener
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for _, kind := range []routeinventory.Listener{routeinventory.ListenerAPI, routeinventory.ListenerHealth, routeinventory.ListenerMetrics} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("tenant matrix loopback bind failed: %s", tenantHarnessSafeError(err))
		}
		listeners = append(listeners, listener)
		addresses[kind] = listener.Addr().String()
	}
	return addresses
}

type tenantChildStderr struct {
	mu   sync.Mutex
	text string
}

func (b *tenantChildStderr) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.text += string(data)
	lines := strings.Split(b.text, "\n")
	if len(lines) > 201 {
		b.text = strings.Join(lines[len(lines)-201:], "\n")
	}
	return len(data), nil
}

func (b *tenantChildStderr) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := strings.Split(strings.TrimSuffix(b.text, "\n"), "\n")
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	return strings.Join(lines, "\n")
}

var tenantLogHex = regexp.MustCompile(`(?i)[0-9a-f]{64}`)
var tenantLogDSN = regexp.MustCompile(`postgres(?:ql)?://[^\s]+`)
var tenantLogAuthorization = regexp.MustCompile(`(?im)^.*authorization.*$`)

func tenantRedactChildLog(value string, rt *tenantRuntime) string {
	secrets := append([]string(nil), rt.secrets...)
	for _, credentials := range rt.creds {
		for _, token := range credentials {
			secrets = append(secrets, token)
		}
	}
	for _, credentials := range rt.freshCreds {
		for _, token := range credentials {
			secrets = append(secrets, token)
		}
	}
	for _, token := range rt.scratchCreds {
		secrets = append(secrets, token)
	}
	if rt.f != nil {
		for _, values := range rt.f.bySide {
			for key, token := range values {
				if tenantSyntheticShapes[key] == "token" {
					secrets = append(secrets, token)
				}
			}
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[credential redacted]")
		}
	}
	value = tenantLogDSN.ReplaceAllString(value, "[database URL redacted]")
	value = tenantLogHex.ReplaceAllString(value, "[digest redacted]")
	return tenantLogAuthorization.ReplaceAllString(value, "[authorization line redacted]")
}

func TestTenantMatrixHarnessGuards(t *testing.T) {
	// Child failures are exactly when diagnostics become tempting. Exercise
	// hostile values from every credential pool, including forgotten secrets
	// bookkeeping, and a database URL never registered as a token.
	rt := &tenantRuntime{
		secrets:      []string{"hostile-explicit-token"},
		creds:        map[tenantSide]map[tenantCred]string{sideB: {credAgent: "hostile-principal-token"}},
		freshCreds:   map[tenantSide]map[string]string{sideA: {"fresh": "hostile-fresh-token"}},
		scratchCreds: map[string]string{"scratch": "hostile-scratch-token"},
		f:            &tenantFixture{bySide: map[tenantSide]map[string]string{sideB: {"token.full": "hostile-fixture-token"}}},
	}
	cases := []struct {
		name, input, forbidden string
	}{
		{"explicit", "message hostile-explicit-token", "hostile-explicit-token"},
		{"principal", "message hostile-principal-token", "hostile-principal-token"},
		{"fresh", "message hostile-fresh-token", "hostile-fresh-token"},
		{"scratch", "message hostile-scratch-token", "hostile-scratch-token"},
		{"fixture", "message hostile-fixture-token", "hostile-fixture-token"},
		{"dsn", "database postgres://account:hostile-password@127.0.0.1/database", "hostile-password"},
		{"postgresql-dsn", "database postgresql://account:hostile-password@127.0.0.1/database", "hostile-password"},
		{"digest", "digest " + strings.Repeat("a1", 32), strings.Repeat("a1", 32)},
		{"authorization", "Authorization: Bearer hostile-unregistered-token", "hostile-unregistered-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tenantRedactChildLog(tc.input, rt), tc.forbidden) {
				t.Fatal("child failure diagnostic retained a sensitive value")
			}
		})
	}
	const reason = "bind: operation not permitted"
	if tenantRedactChildLog(reason, rt) != reason {
		t.Error("safe infrastructure diagnostic was lost")
	}
	stderr := &tenantChildStderr{}
	for i := 0; i < 205; i++ {
		_, _ = fmt.Fprintf(stderr, "line %d\n", i)
	}
	lines := strings.Split(stderr.String(), "\n")
	if len(lines) != 200 || lines[0] != "line 5" || lines[len(lines)-1] != "line 204" {
		t.Error("child stderr does not retain exactly its last 200 lines")
	}
	if len(tenantFingerprintVolatile) != 0 {
		t.Error("tenant matrix must fingerprint every account_id table; no volatile writer has been observed")
	}
}

// A stable snapshot is two equal reads a second apart, with at most ten reads.
// The hashes stay internal; any mismatch diagnostic names only its table.
func tenantFingerprint(t *testing.T, rt *tenantRuntime) map[string]string {
	t.Helper()
	var previous map[string]string
	for read := 0; read < 10; read++ {
		if read > 0 {
			time.Sleep(time.Second)
		}
		current := tenantFingerprintRead(t, rt)
		if read > 0 && reflect.DeepEqual(previous, current) {
			return current
		}
		previous = current
	}
	t.Fatal("tenant matrix account fingerprint did not stabilize in ten reads")
	return nil
}

// tenantFingerprintTables lists the schema's base tables that carry account_id.
func tenantFingerprintTables(ctx context.Context, rt *tenantRuntime) ([]string, error) {
	rows, err := rt.db.QueryContext(ctx, `SELECT c.table_name FROM information_schema.columns c
		JOIN information_schema.tables t ON t.table_schema=c.table_schema AND t.table_name=c.table_name
		WHERE c.table_schema=$1 AND c.column_name='account_id' AND t.table_type='BASE TABLE'
		ORDER BY c.table_name`, rt.schema)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	return tables, rows.Err()
}

func tenantFingerprintRead(t *testing.T, rt *tenantRuntime) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tables, err := tenantFingerprintTables(ctx, rt)
	if err != nil {
		t.Fatalf("tenant matrix fingerprint table discovery failed: %s", tenantHarnessSafeError(err))
	}
	fingerprint := map[string]string{}
	for _, table := range tables {
		if _, excluded := tenantFingerprintVolatile[table]; excluded {
			continue
		}
		var count int64
		var digest string
		query := `SELECT count(*), md5(coalesce(string_agg(row_hash, '' ORDER BY row_hash), ''))
			FROM (SELECT md5(t::text) AS row_hash FROM ` + tenantQuoteIdentifier(rt.schema) + `.` + tenantQuoteIdentifier(table) + ` t WHERE account_id=$1) hashes`
		if err := rt.db.QueryRowContext(ctx, query, rt.f.bySide[sideB]["account"]).Scan(&count, &digest); err != nil {
			t.Fatalf("tenant matrix fingerprint query failed for table %s: %s", table, tenantHarnessSafeError(err))
		}
		fingerprint[table] = fmt.Sprintf("%d:%s", count, digest)
	}
	return fingerprint
}
