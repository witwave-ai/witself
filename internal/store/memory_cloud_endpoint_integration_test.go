package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type managedMemoryCloudEndpoint struct {
	Provider   string
	DSN        string
	ResourceID string
}

type preparedManagedMemoryCloudEndpoint struct {
	spec             managedMemoryCloudEndpoint
	config           *pgx.ConnConfig
	configuredServer string
}

type managedMemoryCloudEndpointEvidence struct {
	Provider          string `json:"provider"`
	Fingerprint       string `json:"fingerprint"`
	PostgreSQLVersion string `json:"postgresql_version"`
}

// managedMemoryCloudArchiveReporter is the certification log boundary for the
// destructive archive round-trip. A pgx/pool error can include a configured
// user, database, host, and port even when it never includes the password. The
// round-trip intentionally has many deep helpers, so certification discards all
// diagnostic payloads at one testing interface rather than relying on every
// call site to remember a substring sanitizer. Rehearsal and ordinary local
// integration tests continue to use *testing.T directly and retain full detail.
type managedMemoryCloudArchiveReporter struct {
	sink                memoryArchiveTestReporter
	sourceProvider      string
	destinationProvider string
}

func newManagedMemoryCloudArchiveReporter(
	sink memoryArchiveTestReporter,
	sourceProvider string,
	destinationProvider string,
) memoryArchiveTestReporter {
	return &managedMemoryCloudArchiveReporter{
		sink:                sink,
		sourceProvider:      safeManagedMemoryCloudProvider(sourceProvider),
		destinationProvider: safeManagedMemoryCloudProvider(destinationProvider),
	}
}

func (r *managedMemoryCloudArchiveReporter) Helper() { r.sink.Helper() }

func (r *managedMemoryCloudArchiveReporter) Cleanup(cleanup func()) {
	r.sink.Cleanup(cleanup)
}

func (r *managedMemoryCloudArchiveReporter) Fatal(_ ...any) {
	r.sink.Helper()
	r.sink.Fatal(r.failureMessage())
}

func (r *managedMemoryCloudArchiveReporter) Fatalf(_ string, _ ...any) {
	r.sink.Helper()
	r.sink.Fatal(r.failureMessage())
}

func (r *managedMemoryCloudArchiveReporter) Errorf(_ string, _ ...any) {
	r.sink.Helper()
	r.sink.Errorf("%s", r.failureMessage())
}

// Successful certification evidence is emitted by the endpoint preflight,
// which logs only a salted fingerprint and server version. Suppress deep helper
// logs as well as errors so later additions cannot accidentally create a second
// topology-bearing log path.
func (r *managedMemoryCloudArchiveReporter) Logf(_ string, _ ...any) {}

func (r *managedMemoryCloudArchiveReporter) failureMessage() string {
	return r.sourceProvider + "_to_" + r.destinationProvider +
		" managed PostgreSQL archive round-trip failed; details suppressed"
}

func safeManagedMemoryCloudProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "aws":
		return "aws"
	case "gcp":
		return "gcp"
	case "azure":
		return "azure"
	default:
		return "unknown"
	}
}

// assertManagedMemoryCloudEndpointsDistinct prevents one database from being
// labeled as three providers in certification evidence. Opaque resource ids
// are operator attestations; configured and live server identities are checked
// independently. Only per-run salted fingerprints and PostgreSQL versions are
// logged, never DSNs, users, passwords, hosts, database names, or resource ids.
func assertManagedMemoryCloudEndpointsDistinct(t *testing.T, specs []managedMemoryCloudEndpoint) []managedMemoryCloudEndpointEvidence {
	t.Helper()
	prepared, err := prepareManagedMemoryCloudEndpoints(specs)
	if err != nil {
		t.Fatal(err)
		return nil
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal("generate cloud certification fingerprint salt")
		return nil
	}
	liveServers := make(map[string]string, len(prepared))
	systemIdentifiers := make(map[string]string, len(prepared))
	evidence := make([]managedMemoryCloudEndpointEvidence, 0, len(prepared))
	for _, endpoint := range prepared {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		conn, err := pgx.ConnectConfig(ctx, endpoint.config.Copy())
		if err != nil {
			cancel()
			t.Fatalf("%s managed PostgreSQL endpoint connection failed; verify the protected DSN and network path", endpoint.spec.Provider)
		}
		var address, database, version, versionNumber string
		var port int
		err = conn.QueryRow(ctx, `
			SELECT COALESCE(inet_server_addr()::text,''),
			       COALESCE(inet_server_port(),0),
			       current_database(),current_setting('server_version'),
			       current_setting('server_version_num')`).Scan(
			&address, &port, &database, &version, &versionNumber,
		)
		systemIdentifier := ""
		if err == nil {
			// pg_control_system is the strongest provider-neutral live cluster
			// identity available. Some managed services intentionally withhold
			// permission, so resource/configured-endpoint attestations remain the
			// mandatory gate and this check is opportunistic.
			_ = conn.QueryRow(ctx, `SELECT system_identifier::text FROM pg_control_system()`).Scan(&systemIdentifier)
		}
		_ = conn.Close(context.Background())
		cancel()
		if err != nil {
			t.Fatalf("%s managed PostgreSQL endpoint identity query failed", endpoint.spec.Provider)
		}
		if address == "" {
			address = endpoint.config.Host
		}
		if port == 0 {
			port = int(endpoint.config.Port)
		}
		liveServer := normalizedMemoryCloudServer(address, port)
		if other, exists := liveServers[liveServer]; exists {
			// Private networks can legitimately reuse RFC1918 address/port pairs.
			// Preserve the collision as evidence without treating it as provider
			// identity or rejecting otherwise distinct attested resources.
			t.Logf("warning: %s and %s report the same private server address/port; relying on distinct configured endpoints and resource attestations",
				other, endpoint.spec.Provider)
		}
		liveServers[liveServer] = endpoint.spec.Provider
		if systemIdentifier != "" {
			if other, exists := systemIdentifiers[systemIdentifier]; exists {
				t.Fatalf("managed cloud endpoints %s and %s report the same PostgreSQL system identifier", other, endpoint.spec.Provider)
			}
			systemIdentifiers[systemIdentifier] = endpoint.spec.Provider
		} else {
			t.Logf("%s PostgreSQL system identifier is unavailable; relying on configured endpoint and resource attestation", endpoint.spec.Provider)
		}
		fingerprint := managedMemoryCloudFingerprint(
			salt, endpoint.spec.Provider, endpoint.spec.ResourceID,
			endpoint.configuredServer, liveServer, database, systemIdentifier,
		)
		t.Logf("%s managed PostgreSQL endpoint fingerprint %s; PostgreSQL %s",
			endpoint.spec.Provider, fingerprint, version)
		evidence = append(evidence, managedMemoryCloudEndpointEvidence{
			Provider: endpoint.spec.Provider, Fingerprint: fingerprint, PostgreSQLVersion: versionNumber,
		})
	}
	return evidence
}

func prepareManagedMemoryCloudEndpoints(specs []managedMemoryCloudEndpoint) ([]preparedManagedMemoryCloudEndpoint, error) {
	if len(specs) != 3 {
		return nil, errors.New("managed cloud certification requires exactly three endpoints")
	}
	providers := map[string]bool{"aws": true, "gcp": true, "azure": true}
	seenProviders := make(map[string]bool, len(specs))
	seenResources := make(map[string]string, len(specs))
	seenServers := make(map[string]string, len(specs))
	prepared := make([]preparedManagedMemoryCloudEndpoint, 0, len(specs))
	for _, spec := range specs {
		provider := strings.ToLower(strings.TrimSpace(spec.Provider))
		if !providers[provider] || seenProviders[provider] {
			return nil, errors.New("managed cloud certification requires one AWS, GCP, and Azure endpoint")
		}
		seenProviders[provider] = true
		resourceID := strings.TrimSpace(spec.ResourceID)
		if resourceID == "" || len(resourceID) > 2048 || strings.ContainsAny(resourceID, "\x00\r\n") {
			return nil, fmt.Errorf("%s managed cloud resource attestation is invalid", provider)
		}
		resourceKey := strings.ToLower(resourceID)
		if other, exists := seenResources[resourceKey]; exists {
			return nil, fmt.Errorf("managed cloud providers %s and %s use the same resource attestation", other, provider)
		}
		seenResources[resourceKey] = provider
		config, err := pgx.ParseConfig(spec.DSN)
		if err != nil {
			return nil, fmt.Errorf("%s managed PostgreSQL DSN is invalid", provider)
		}
		host := strings.TrimSpace(config.Host)
		if host == "" || strings.HasPrefix(host, "/") || config.Port == 0 {
			return nil, fmt.Errorf("%s managed PostgreSQL DSN must name a TCP host and port", provider)
		}
		configuredServer := normalizedMemoryCloudServer(host, int(config.Port))
		if other, exists := seenServers[configuredServer]; exists {
			return nil, fmt.Errorf("managed cloud providers %s and %s use the same configured PostgreSQL server", other, provider)
		}
		seenServers[configuredServer] = provider
		prepared = append(prepared, preparedManagedMemoryCloudEndpoint{
			spec:   managedMemoryCloudEndpoint{Provider: provider, DSN: spec.DSN, ResourceID: resourceID},
			config: config, configuredServer: configuredServer,
		})
	}
	return prepared, nil
}

func normalizedMemoryCloudServer(host string, port int) string {
	return strings.ToLower(net.JoinHostPort(strings.TrimSpace(host), strconv.Itoa(port)))
}

func managedMemoryCloudFingerprint(salt []byte, values ...string) string {
	digest := sha256.New()
	_, _ = digest.Write(salt)
	for _, value := range values {
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(value))
	}
	return hex.EncodeToString(digest.Sum(nil))[:16]
}

func TestPrepareManagedMemoryCloudEndpointsRejectsAliasCertification(t *testing.T) {
	base := []managedMemoryCloudEndpoint{
		{Provider: "aws", DSN: "postgres://user:secret-a@aws.invalid:5432/witself", ResourceID: "aws-resource"},
		{Provider: "gcp", DSN: "postgres://user:secret-b@gcp.invalid:5432/witself", ResourceID: "gcp-resource"},
		{Provider: "azure", DSN: "postgres://user:secret-c@azure.invalid:5432/witself", ResourceID: "azure-resource"},
	}
	if prepared, err := prepareManagedMemoryCloudEndpoints(base); err != nil || len(prepared) != 3 {
		t.Fatalf("distinct endpoints = %d / %v", len(prepared), err)
	}

	t.Run("same configured server", func(t *testing.T) {
		aliased := append([]managedMemoryCloudEndpoint(nil), base...)
		aliased[1].DSN = "postgres://different:do-not-log@aws.invalid:5432/other_database"
		_, err := prepareManagedMemoryCloudEndpoints(aliased)
		if err == nil || !strings.Contains(err.Error(), "same configured PostgreSQL server") || strings.Contains(err.Error(), "do-not-log") {
			t.Fatalf("alias error = %v", err)
		}
	})

	t.Run("same resource attestation", func(t *testing.T) {
		aliased := append([]managedMemoryCloudEndpoint(nil), base...)
		aliased[2].ResourceID = base[0].ResourceID
		_, err := prepareManagedMemoryCloudEndpoints(aliased)
		if err == nil || !strings.Contains(err.Error(), "same resource attestation") {
			t.Fatalf("resource alias error = %v", err)
		}
	})
}

const managedMemoryCloudRedactionHelperDSN = "WITSELF_TEST_MEMORY_CLOUD_REDACTION_HELPER_DSN"

// TestManagedMemoryCloudArchiveReporterRedactsClosedEndpointFailure executes the
// real schema-setup path in a child test process. The endpoint is dynamically
// allocated and closed before the child starts, guaranteeing a pgx connection
// error whose native text contains topology fields. Only the fixed provider-pair
// and archive-stage message may cross the certification reporter.
func TestManagedMemoryCloudArchiveReporterRedactsClosedEndpointFailure(t *testing.T) {
	if dsn := os.Getenv(managedMemoryCloudRedactionHelperDSN); dsn != "" {
		reporter := newManagedMemoryCloudArchiveReporter(t, "aws", "gcp")
		runNarrativeMemoryArchiveCellMoveWithReporter(
			reporter, dsn, dsn, "aws-managed-postgres", "gcp-managed-postgres",
		)
		t.Fatal("closed-endpoint redaction helper unexpectedly succeeded")
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	dsn := "postgres://cloud_log_user:cloud_log_password@" + net.JoinHostPort(host, port) +
		"/cloud_log_database?sslmode=disable&application_name=cloud_resource_system_marker"
	command := exec.Command(
		os.Args[0],
		"-test.run=^TestManagedMemoryCloudArchiveReporterRedactsClosedEndpointFailure$",
		"-test.v",
	)
	command.Env = append(os.Environ(), managedMemoryCloudRedactionHelperDSN+"="+dsn)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("closed-endpoint helper unexpectedly passed:\n%s", output)
	}
	text := string(output)
	want := "aws_to_gcp managed PostgreSQL archive round-trip failed; details suppressed"
	if !strings.Contains(text, want) {
		t.Fatalf("redacted failure missing %q:\n%s", want, text)
	}
	for _, forbidden := range []string{
		dsn,
		"cloud_log_user",
		"cloud_log_password",
		"cloud_log_database",
		host,
		port,
		"cloud_resource_system_marker",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("certification failure output contains protected endpoint field %q:\n%s", forbidden, text)
		}
	}
}
