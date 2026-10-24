package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/agentemailoutbound"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/worker"
)

func localWorkerValues(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"WITSELF_RUNTIME": "local", "WITSELF_DATABASE_URL": "postgres://synthetic.invalid/local", "WITSELF_LOCAL_SPOOL_DIR": dir}
}

func TestWorkerRuntimeDispatchGuard(t *testing.T) {
	commands := []struct {
		name    string
		args    []string
		allowed bool
	}{
		{"version", []string{"version"}, false},
		{"--version", []string{"--version"}, true},
		{"-v", []string{"-v"}, true},
		{"help", []string{"help"}, false},
		{"--help", []string{"--help"}, true},
		{"-h", []string{"-h"}, true},
		{"serve", []string{"serve"}, true},
		{"agent-email", []string{"agent-email", "receipt-replay"}, false},
		{"unknown", []string{"unknown"}, false},
		{"empty", nil, false},
	}
	// Keep the table exhaustive when the actual command dispatcher grows.
	source, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	covered := make(map[string]bool)
	for _, tc := range commands {
		covered[tc.name] = true
	}
	for _, decl := range source.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runWith" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			clause, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				literal, ok := expr.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatal("dispatch test needs a case for a nonliteral command")
				}
				name, err := strconv.Unquote(literal.Value)
				if err != nil || !covered[name] {
					t.Fatalf("dispatch table does not cover %s", literal.Value)
				}
			}
			return true
		})
	}
	for _, selected := range []string{"local", "unknown-private-marker"} {
		for _, tc := range commands {
			t.Run(selected+"/"+tc.name, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				lookups, serveCalls := 0, 0
				lookup := func(name string) (string, bool) {
					if name != "WITSELF_RUNTIME" {
						t.Fatal("dispatcher consulted another variable before refusal")
					}
					lookups++
					return selected, true
				}
				code := runWith(tc.args, lookup, &stdout, &stderr, func() int {
					if !tc.allowed {
						t.Fatal("refused command reached the serve store path")
					}
					serveCalls++
					return 0
				}, func([]string) int {
					t.Fatal("refused command reached the receipt-replay store/client path")
					return 0
				})
				if lookups != 1 {
					t.Fatalf("runtime lookup count = %d, want 1", lookups)
				}
				if tc.allowed {
					if code != 0 || stderr.Len() != 0 || (serveCalls == 1) != (tc.name == "serve") {
						t.Fatalf("allowed command dispatch changed: code=%d, serve calls=%d", code, serveCalls)
					}
				} else if code != 1 || stdout.Len() != 0 || stderr.String() != "operational subcommands are unavailable while WITSELF_RUNTIME is set\n" {
					t.Fatal("operational command did not receive the exact value-free refusal")
				}
			})
		}
	}
}

func TestWorkerHostedRegistrationLogs(t *testing.T) {
	const defaults = "witself-worker: avatar style rollout enabled (batch 100, interval 2s, timeout 30s)\n" +
		"witself-worker: message rate bucket cleanup enabled (batch 10000, interval 1m0s, timeout 10s)\n" +
		"witself-worker: agent-email rate bucket cleanup enabled (batch 10000, interval 1m0s, timeout 10s)\n"
	for _, tc := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{name: "defaults", want: defaults},
		{name: "opt-in transcript retention", values: map[string]string{transcriptRetentionEnabledEnv: "true"}, want: defaults + "witself-worker: transcript retention enabled (mode preview, batch 100, interval 5m0s, timeout 2m0s)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, jobs, err := workerConfigFromEnv(mapLookup(tc.values), nil)
			if err != nil {
				t.Fatal(err)
			}
			got := captureWorkerStderr(t, func() {
				if code := registerJobs(worker.NewRegistry(), nil, jobs); code != 0 {
					t.Fatal("hosted registration failed")
				}
			})
			if got != tc.want {
				t.Fatalf("hosted registration log changed:\ngot: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

type workerBootstrapStore struct {
	version int
	account string
	ping    func(context.Context) error
}

func (s workerBootstrapStore) Ping(ctx context.Context) error {
	if s.ping != nil {
		return s.ping(ctx)
	}
	return nil
}

func (s workerBootstrapStore) AppliedSchemaVersion(context.Context) (int, error) {
	return s.version, nil
}

func (s workerBootstrapStore) LocalBootstrapAccount(context.Context) (string, error) {
	return s.account, nil
}

func TestWorkerLocalServeWiring(t *testing.T) {
	for _, tc := range []struct {
		name           string
		account        string
		version        int
		wantRun, ready bool
	}{
		{name: "account mismatch before registry", account: "other-private-account", version: store.SchemaVersion()},
		{name: "schema mismatch", account: "expected-private-account", version: store.SchemaVersion() - 1, wantRun: true},
		{name: "schema equal", account: "expected-private-account", version: store.SchemaVersion(), wantRun: true, ready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := runtimeconfig.Config{Local: true, AccountID: "expected-private-account", HealthAddr: "127.0.0.1:8082", MetricsAddr: "127.0.0.1:9091"}
			st := workerBootstrapStore{version: tc.version, account: tc.account}
			ran := false
			output := captureWorkerStderr(t, func() {
				code := serveWorker(context.Background(), nil, st, runtime, jobConfig{}, mapLookup(nil), func(_ *worker.Registry, ctx context.Context, cfg worker.Config) error {
					ran = true
					if !tc.wantRun {
						t.Fatal("account mismatch reached registry.Run")
					}
					if cfg.HealthAddr != runtime.HealthAddr || cfg.MetricsAddr != runtime.MetricsAddr {
						t.Fatal("local listener configuration did not reach registry.Run")
					}
					if err := cfg.Ready(ctx); (err == nil) != tc.ready {
						t.Fatalf("selected readiness success = %t, want %t", err == nil, tc.ready)
					}
					return nil
				})
				if (code == 0) != tc.wantRun || ran != tc.wantRun {
					t.Fatalf("serve exit=%d, registry ran=%t", code, ran)
				}
			})
			if strings.Contains(output, "private-account") {
				t.Fatal("account refusal disclosed a database value")
			}
		})
	}
}

func TestWorkerLocalReadinessDeadline(t *testing.T) {
	st := workerBootstrapStore{account: "test-account", version: store.SchemaVersion(), ping: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	captureWorkerStderr(t, func() {
		code := serveWorker(context.Background(), nil, st, runtimeconfig.Config{Local: true}, jobConfig{}, mapLookup(nil), func(_ *worker.Registry, _ context.Context, cfg worker.Config) error {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			started := time.Now()
			go func() { result <- cfg.Ready(ctx) }()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("blocking store unexpectedly became ready")
				}
				if elapsed := time.Since(started); elapsed < 1900*time.Millisecond || elapsed > 2500*time.Millisecond {
					t.Fatalf("readiness deadline elapsed %s, want two seconds", elapsed)
				}
			case <-time.After(2500 * time.Millisecond):
				t.Fatal("local readiness exceeded the two-second bound")
			}
			return nil
		})
		if code != 0 {
			t.Fatal("local worker startup failed")
		}
	})
}

func captureWorkerStderr(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	previous := os.Stderr
	os.Stderr = writer
	defer func() {
		os.Stderr = previous
		_ = writer.Close()
	}()
	fn()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestWorkerRuntimeSelector(t *testing.T) {
	for _, value := range []string{"unset", "", "local", "unknown-private-marker"} {
		t.Run(value, func(t *testing.T) {
			values := localWorkerValues(t)
			if value == "unset" {
				delete(values, "WITSELF_RUNTIME")
			} else {
				values["WITSELF_RUNTIME"] = value
			}
			runtime, _, err := workerConfigFromEnv(mapLookup(values), nil)
			if value == "unknown-private-marker" {
				if err == nil || !strings.Contains(err.Error(), "WITSELF_RUNTIME") || strings.Contains(err.Error(), value) {
					t.Fatalf("unsafe refusal: %v", err)
				}
			} else if err != nil || runtime.Local != (value == "local") {
				t.Fatalf("selection: %v", err)
			}
		})
	}
}

func TestWorkerHostedConfigurationSnapshot(t *testing.T) {
	runtime, jobs, err := workerConfigFromEnv(mapLookup(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := jobConfig{
		avatarEnabled:                      true,
		avatar:                             store.AvatarStyleRolloutWorkerConfig{BatchSize: 100, Interval: 2 * time.Second, BatchTimeout: 30 * time.Second},
		messageRateBucketCleanupEnabled:    true,
		messageRateBucketCleanup:           store.MessageRateBucketCleanupWorkerConfig{BatchSize: 10000, Interval: time.Minute, BatchTimeout: 10 * time.Second},
		agentEmailRateBucketCleanupEnabled: true,
		agentEmailRateBucketCleanup:        store.AgentEmailRateBucketCleanupWorkerConfig{BatchSize: 10000, Interval: time.Minute, BatchTimeout: 10 * time.Second},
		agentEmailOutbound:                 agentEmailOutboundWorkerConfig{BatchSize: 10, Interval: 2 * time.Second, BatchTimeout: 30 * time.Second, ProviderTimeout: 20 * time.Second},
		agentEmailOutboundClient:           agentemailoutbound.Client{Audience: "witself-agent-email-send"},
		retention:                          store.TranscriptRetentionWorkerConfig{BatchSize: 100, Interval: 5 * time.Minute, BatchTimeout: 2 * time.Minute, LaneCount: 16, Mode: "preview"},
		messageRetention:                   store.MessageRetentionWorkerConfig{BatchSize: 25, Interval: 5 * time.Minute, BatchTimeout: 2 * time.Minute, LaneCount: 16, Mode: "preview"},
		agentEmailRetention:                store.AgentEmailRetentionWorkerConfig{BatchSize: 25, Interval: 5 * time.Minute, BatchTimeout: 2 * time.Minute, LaneCount: 16, Mode: "preview"},
		accountPurge:                       store.AccountPurgeWorkerConfig{BatchSize: 100, Interval: 5 * time.Minute, BatchTimeout: 2 * time.Minute, Grace: 720 * time.Hour, Mode: "preview"},
		supportTicketAgeOut:                store.SupportTicketAgeOutWorkerConfig{After: 720 * time.Hour, BatchSize: 100, Interval: time.Hour, BatchTimeout: 10 * time.Second},
		transcriptStorageReconcile:         store.TranscriptStorageReconcileConfig{Interval: 5 * time.Minute, DutyCycle: .5, PageEntries: 1000, UnitTimeout: 240 * time.Second},
	}
	if !reflect.DeepEqual(runtime, runtimeconfig.Config{}) || !reflect.DeepEqual(jobs, want) {
		t.Fatal("hosted job configuration changed from pre-local snapshot")
	}
	health, metrics := workerListenerAddresses(runtime, mapLookup(nil))
	if health != ":8081" || metrics != ":9090" {
		t.Fatal("hosted listener defaults changed")
	}
	values := map[string]string{"WITSELF_HEALTH_ADDR": "  :8101  ", "WITSELF_METRICS_ADDR": " :9101 ", "WITSELF_LOCAL_JOBS": "unknown-local-only-setting"}
	runtime, _, err = workerConfigFromEnv(mapLookup(values), nil)
	if err != nil {
		t.Fatal(err)
	}
	health, metrics = workerListenerAddresses(runtime, mapLookup(values))
	if health != ":8101" || metrics != ":9101" {
		t.Fatal("hosted overrides changed")
	}
}

func TestWorkerLocalJobsRegisterOnlySelected(t *testing.T) {
	allowed := []string{avatarStyleRolloutJob, messageRateBucketCleanupJob, agentEmailRateBucketCleanupJob, transcriptStorageReconcileJob}
	all := append(append([]string{}, allowed...), agentEmailOutboundJob, accountPurgeJob, transcriptRetentionJob, messageRetentionJob, agentEmailRetentionJob, supportTicketAgeOutJob)
	for _, selected := range append([]string{"", strings.Join(allowed, ",")}, allowed...) {
		t.Run(selected, func(t *testing.T) {
			values := localWorkerValues(t)
			values["WITSELF_LOCAL_JOBS"] = selected
			// The explicit selection overrides both enabled and disabled ambient switches.
			for _, key := range []string{avatarStyleRolloutEnabledEnv, messageRateBucketCleanupEnabledEnv, agentEmailRateBucketCleanupEnabledEnv, transcriptStorageReconcileEnv + "ENABLED"} {
				values[key] = "false"
			}
			for _, key := range []string{agentEmailOutboundEnabledEnv, accountPurgeEnabledEnv, transcriptRetentionEnabledEnv, messageRetentionEnabledEnv, agentEmailRetentionEnabledEnv, "WITSELF_SUPPORT_TICKET_AGE_OUT_ENABLED"} {
				values[key] = "true"
			}
			runtime, jobs, err := workerConfigFromEnv(mapLookup(values), nil)
			if err != nil {
				t.Fatal(err)
			}
			health, metrics := workerListenerAddresses(runtime, mapLookup(values))
			if health != "127.0.0.1:8082" || metrics != "127.0.0.1:9091" {
				t.Fatal("local listener defaults not applied")
			}
			registry := worker.NewRegistry()
			if code := registerJobs(registry, nil, jobs); code != 0 {
				t.Fatal("job registration failed")
			}
			for _, name := range all {
				// Duplicate registration is a public, inert probe of actual registered names.
				err := registry.Register(worker.Job{Name: name, Run: func(context.Context) error { return nil }})
				want := selected != "" && strings.Contains(","+selected+",", ","+name+",")
				if (err != nil) != want {
					t.Fatalf("registration of %s: selected=%t already registered=%t", name, want, err != nil)
				}
			}
		})
	}
}

func TestWorkerLocalJobsRefuseForbiddenAndUnknown(t *testing.T) {
	for _, name := range []string{agentEmailOutboundJob, accountPurgeJob, transcriptRetentionJob, messageRetentionJob, agentEmailRetentionJob, supportTicketAgeOutJob, "unknown-private-marker"} {
		t.Run(name, func(t *testing.T) {
			values := localWorkerValues(t)
			values["WITSELF_LOCAL_JOBS"] = name
			_, _, err := workerConfigFromEnv(mapLookup(values), nil)
			if err == nil || !strings.Contains(err.Error(), "WITSELF_LOCAL_JOBS") || strings.Contains(err.Error(), name) {
				t.Fatalf("unsafe job refusal: %v", err)
			}
		})
	}
}

// The production entry point must consult the selector itself; the guard tests
// above exercise runWith with an injected lookup, so a disconnected caller would
// survive them.
func TestWorkerRunEntryHonorsRuntimeSelector(t *testing.T) {
	for _, selector := range []string{"local", "private-unknown-runtime"} {
		t.Run(selector, func(t *testing.T) {
			t.Setenv("WITSELF_RUNTIME", selector)
			for _, args := range [][]string{{"version"}, {"agent-email", "receipt-replay"}, nil} {
				if code := run(args); code != 1 {
					t.Fatalf("run(%q) under selector %q = %d, want 1", args, selector, code)
				}
			}
		})
	}
	t.Setenv("WITSELF_RUNTIME", "")
	if code := run([]string{"version"}); code != 0 {
		t.Fatalf("hosted run(version) = %d, want 0", code)
	}
}
