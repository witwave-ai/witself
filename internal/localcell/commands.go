package localcell

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func (p Ports) all() []int {
	return []int{p.Postgres, p.ServerAPI, p.ServerHealth, p.ServerMetrics, p.WorkerHealth, p.WorkerMetrics}
}

func allocatePorts() (Ports, error) {
	listeners := make([]net.Listener, 0, 6)
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	ports := make([]int, 0, 6)
	for range 6 {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return Ports{}, errors.New("cannot allocate local cell loopback ports")
		}
		listeners = append(listeners, listener)
		ports = append(ports, listener.Addr().(*net.TCPAddr).Port)
	}
	return Ports{ports[0], ports[1], ports[2], ports[3], ports[4], ports[5]}, nil
}

func addr(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

func (c *Cell) password() (string, error) {
	path := c.path("run", "postgres.password")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128 || !privatePassword(path, info) {
		return "", errors.New("local cell password file is unavailable or not private")
	}
	data, err := readPrivateFile(path)
	if err != nil || len(bytes.TrimSpace(data)) < 24 {
		return "", errors.New("local cell password file is invalid")
	}
	return strings.TrimSpace(string(data)), nil
}

func (c *Cell) makePassword() error {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return errors.New("cannot generate local cell password")
	}
	return writeAtomicBytes(c.path("run", "postgres.password"), []byte(base64.RawURLEncoding.EncodeToString(data)+"\n"), nil)
}

func (c *Cell) childEnvironment(m *Manifest, component string) ([]string, error) {
	password, err := c.password()
	if err != nil {
		return nil, err
	}
	dsn := url.URL{Scheme: "postgres", Host: addr(m.Ports.Postgres), Path: "/" + localDatabaseName, User: url.UserPassword(m.Role, password), RawQuery: "sslmode=disable"}
	health, metrics := m.Ports.ServerHealth, m.Ports.ServerMetrics
	if component == "worker" {
		health, metrics = m.Ports.WorkerHealth, m.Ports.WorkerMetrics
	}
	environment, err := c.baseEnvironment()
	if err != nil {
		return nil, err
	}
	environment = appendEnvironment(environment,
		"WITSELF_RUNTIME=local",
		"WITSELF_DATABASE_URL="+dsn.String(),
		"WITSELF_API_ADDR="+addr(m.Ports.ServerAPI),
		"WITSELF_HEALTH_ADDR="+addr(health),
		"WITSELF_METRICS_ADDR="+addr(metrics),
		"WITSELF_LOCAL_SPOOL_DIR="+c.path("spool"),
		"WITSELF_LOCAL_JOBS="+strings.Join(c.cfg.Jobs, ","),
	)
	if m.AccountID != "" {
		environment = appendEnvironment(environment, "WITSELF_LOCAL_ACCOUNT_ID="+m.AccountID)
	}
	return environment, nil
}

func (c *Cell) command(ctx context.Context, binary string, args ...string) (*exec.Cmd, error) {
	environment, err := c.baseEnvironment()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = environment
	cmd.Dir = c.cfg.Root
	return cmd, nil
}

func (c *Cell) logFile(component string) (*os.File, error) {
	path := c.path("logs", component+".log")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("local cell log must be a regular file")
	}
	f, err := openPrivateFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, errors.New("cannot open local cell log")
	}
	return f, nil
}

func (c *Cell) pgCommand(ctx context.Context, tool string, args ...string) error {
	if !filepath.IsAbs(c.cfg.Binaries.PostgresDir) {
		return errors.New("explicit PostgreSQL binary directory is required")
	}
	log, err := c.logFile("postgres")
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	cmd, err := c.command(ctx, filepath.Join(c.cfg.Binaries.PostgresDir, executable(tool)), args...)
	if err != nil {
		return err
	}
	cmd.Env, err = c.postgresEnvironment()
	if err != nil {
		return err
	}
	// PostgreSQL tools need a valid locale; on macOS a postmaster started without
	// one aborts ("became multithreaded during startup"). The server and worker
	// children keep the exact allowlist and do not receive these.
	cmd.Env = appendEnvironment(cmd.Env, "LC_ALL=C", "LANG=C")
	cmd.Stdout, cmd.Stderr = log, log
	release, err := configurePostgresCommand(cmd)
	if err != nil {
		return err
	}
	defer release()
	if cmd.Run() != nil {
		return errors.New("local cell PostgreSQL command failed")
	}
	return nil
}

func executable(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func (c *Cell) reportedVersion(ctx context.Context, binary string) (string, error) {
	if !filepath.IsAbs(binary) {
		return "", errors.New("component binary is required")
	}
	cmd, err := c.command(ctx, binary, "--version")
	if err != nil {
		return "", err
	}
	release, err := configurePostgresVersionCommand(cmd, c.cfg.Binaries.PostgresDir)
	if err != nil {
		return "", err
	}
	defer release()
	var output bytes.Buffer
	cmd.Stdout = &output
	if cmd.Run() != nil || output.Len() > 1024 {
		return "", errors.New("cannot read component version")
	}
	v := strings.TrimSpace(output.String())
	if v == "" || strings.ContainsAny(v, "\r\n") {
		return "", errors.New("invalid component version")
	}
	return v, nil
}

func (c *Cell) configurePostgres(m *Manifest) error {
	// Loopback TCP only: every connection the lifecycle makes uses 127.0.0.1,
	// and a Unix socket under a long root would exceed macOS's 104-byte limit.
	socketDir := ""
	quote := func(value string) string {
		return "'" + strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "'", "''") + "'"
	}
	config := fmt.Sprintf("listen_addresses = '127.0.0.1'\nport = %d\nunix_socket_directories = %s\npassword_encryption = 'scram-sha-256'\nlogging_collector = off\nlog_destination = 'stderr'\n", m.Ports.Postgres, quote(socketDir))
	return c.writePostgresConfig([]byte(config))
}

func (c *Cell) databaseCommand(ctx context.Context, _ *Manifest, tool string, args ...string) (string, error) {
	if !filepath.IsAbs(c.cfg.Binaries.PostgresDir) {
		return "", errors.New("explicit PostgreSQL binary directory is required")
	}
	password, err := c.password()
	if err != nil {
		return "", err
	}
	cmd, err := c.command(ctx, filepath.Join(c.cfg.Binaries.PostgresDir, executable(tool)), args...)
	if err != nil {
		return "", err
	}
	cmd.Env, err = c.postgresEnvironment()
	if err != nil {
		return "", err
	}
	cmd.Env = appendEnvironment(cmd.Env, "PGPASSWORD="+password, "PGCONNECT_TIMEOUT=3", "LC_ALL=C", "LANG=C")
	release, err := configurePostgresCommand(cmd)
	if err != nil {
		return "", err
	}
	defer release()
	var output bytes.Buffer
	cmd.Stdout = &output
	// Do not propagate database errors, SQL, or connection strings to callers.
	if cmd.Run() != nil || output.Len() > 4096 {
		return "", errors.New("local cell database command failed")
	}
	return strings.TrimSpace(output.String()), nil
}

func (c *Cell) psql(ctx context.Context, m *Manifest, query string) (string, error) {
	return c.databaseCommand(ctx, m, "psql", "--no-psqlrc", "--no-password", "-h", "127.0.0.1", "-p", strconv.Itoa(m.Ports.Postgres), "-U", m.Role, "-d", localDatabaseName, "-At", "-v", "ON_ERROR_STOP=1", "-c", query)
}

func (c *Cell) startChild(m *Manifest, component string) error {
	state, err := c.processState(component)
	if err != nil {
		return err
	}
	if state.Alive {
		if !state.Matches {
			return errors.New("local cell process identity mismatch")
		}
		return nil
	}
	binary := c.cfg.Binaries.Server
	if component == "worker" {
		binary = c.cfg.Binaries.Worker
	}
	if binary == "" {
		return errors.New("component binary is required")
	}
	environment, err := c.childEnvironment(m, component)
	if err != nil {
		return err
	}
	log, err := c.logFile(component)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	return c.launchChild(binary, component, environment, log)
}

func ready(ctx context.Context, client *http.Client, port int) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr(port)+"/readyz", nil)
	if err != nil {
		return false
	}
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 128))
	return res.StatusCode == http.StatusOK
}

// readinessError retains the observation made before Start rolls back its
// children and removes their records. Its text is deliberately restricted to
// fixed messages; no component path, process identity, or underlying error is
// included in the public error.
type readinessError struct {
	component         string
	deadlineExceeded  bool
	state             ProcessState
	observationFailed bool
}

func (e *readinessError) Error() string {
	switch e.component {
	case "server":
		if e.deadlineExceeded {
			return "local cell server readiness deadline exceeded"
		}
		return "local cell server exited before readiness"
	case "worker":
		if e.deadlineExceeded {
			return "local cell worker readiness deadline exceeded"
		}
		return "local cell worker exited before readiness"
	default:
		return "local cell readiness failed"
	}
}

func (c *Cell) waitReady(ctx context.Context, component string, port int) error {
	client := localHTTPClient()
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := c.processState(component)
		if err != nil || !state.Alive || !state.Matches {
			return &readinessError{component: component, state: state, observationFailed: err != nil}
		}
		if ready(ctx, client, port) {
			return nil
		}
		select {
		case <-ctx.Done():
			return &readinessError{component: component, deadlineExceeded: true, state: state}
		case <-ticker.C:
		}
	}
}

var postgresVersionPattern = regexp.MustCompile(`\b([0-9]+\.[0-9]+)(?:\.[0-9]+)?\b`)
var accountPattern = regexp.MustCompile(`^acc_[a-z2-7]+$`)
