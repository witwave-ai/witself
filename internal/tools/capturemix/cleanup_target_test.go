package capturemix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/testenv"
	"go.yaml.in/yaml/v3"
)

// Cleanup regressions allocate only their own schemas and per-test roles. They
// use the general test harness target policy rather than the executable matrix's
// narrower rehearsal policy. This constructor is absent from production builds.
func openCleanupTestCluster(ctx context.Context, dsn string, connect func(context.Context, string) (*pgx.Conn, error)) (*cluster, error) {
	if testenv.ValidateHarnessPostgresDSN(dsn) != nil {
		return nil, errors.New("capturemix cleanup database target refused")
	}
	admin, err := connect(ctx, dsn)
	if err != nil {
		return nil, errors.New("capturemix cleanup database unavailable")
	}
	return &cluster{admin: admin, dsn: dsn, passwords: map[string]string{}}, nil
}

const cleanupTargetChildEnv = "WITSELF_CAPTUREMIX_CLEANUP_TARGET_CHILD"

// Only the parent-created Go overlay changes this constant. An ambient child
// flag must never select production connection tests without the spy installed.
const cleanupTargetOverlayActive = false

var cleanupTargetProductionConnectCalls int
var cleanupTargetProductionConnectDSN string

//nolint:unused // Called by the temporary production-source overlay below.
func cleanupTargetProductionConnect(_ context.Context, dsn string) (*pgx.Conn, error) {
	cleanupTargetProductionConnectCalls++
	cleanupTargetProductionConnectDSN = dsn
	return nil, errors.New("capturemix inert connection spy")
}

func TestCaptureMixCleanupTargetsHermetic(t *testing.T) {
	child := os.Getenv(cleanupTargetChildEnv) == "1"
	if child != cleanupTargetOverlayActive {
		t.Fatal("capturemix cleanup target child requires its connection overlay")
	}
	t.Setenv("WITSELF_SYNC_HARNESS_DB", "")
	t.Run("source_boundary", assertCleanupTargetSourceBoundary)
	ciTarget := cleanupGoRestTarget(t)
	targets := []struct {
		name, dsn         string
		productionAllowed bool
	}{
		{name: "go_rest", dsn: ciTarget},
		{name: "gate_a", dsn: cleanupTargetURL("127.0.0.1:5599", "gate_a")},
		{name: "codex_a", dsn: cleanupTargetURL("127.0.0.1:5599", "codex_a"), productionAllowed: true},
		{name: "codex_b", dsn: cleanupTargetURL("127.0.0.1:5599", "codex_b"), productionAllowed: true},
		{name: "codex_c", dsn: cleanupTargetURL("127.0.0.1:5599", "codex_c"), productionAllowed: true},
		{name: "codex_d", dsn: cleanupTargetURL("127.0.0.1:5599", "codex_d"), productionAllowed: true},
		{name: "other_local_harness", dsn: cleanupTargetURL("localhost:5432", "cleanup_test")},
		{name: "ipv6_harness", dsn: cleanupTargetURL("[::1]:5432", "cleanup_test")},
	}
	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			if testenv.ValidateHarnessPostgresDSN(target.dsn) != nil {
				t.Fatal("capturemix cleanup target no longer passes harness validation")
			}
			parsed, err := url.Parse(target.dsn)
			if err != nil || allowedClusterTarget(parsed, false) != target.productionAllowed {
				t.Fatal("capturemix cleanup target production allowlist changed")
			}
			fixtureCalls := 0
			fixture, err := openCleanupTestCluster(context.Background(), target.dsn, func(_ context.Context, dsn string) (*pgx.Conn, error) {
				fixtureCalls++
				if dsn != target.dsn {
					t.Fatal("capturemix cleanup fixture changed the validated target")
				}
				return nil, errors.New("capturemix inert connection spy")
			})
			if fixture != nil || err == nil || err.Error() != "capturemix cleanup database unavailable" || fixtureCalls != 1 {
				t.Fatal("capturemix cleanup fixture did not reach exactly one inert connection")
			}
			if child {
				cleanupTargetProductionConnectCalls = 0
				cleanupTargetProductionConnectDSN = ""
				production, err := openCluster(context.Background(), Options{OwnerDSN: target.dsn})
				wantCalls := 0
				wantError := "capturemix database target refused"
				wantDSN := ""
				if target.productionAllowed {
					wantCalls = 1
					wantError = "capturemix database unavailable"
					wantDSN = target.dsn
				}
				if production != nil || err == nil || err.Error() != wantError || cleanupTargetProductionConnectCalls != wantCalls || cleanupTargetProductionConnectDSN != wantDSN {
					t.Fatal("capturemix production connection boundary changed")
				}
			}
		})
	}
	for _, target := range []struct{ name, dsn string }{
		{"remote", cleanupTargetURL("example.invalid:5599", "codex_b")},
		{"target_override", cleanupTargetURL("127.0.0.1:5599", "codex_b") + "&host=example.invalid"},
		{"malformed", "not-a-database-target"},
	} {
		t.Run(target.name, func(t *testing.T) {
			calls := 0
			fixture, err := openCleanupTestCluster(context.Background(), target.dsn, func(context.Context, string) (*pgx.Conn, error) {
				calls++
				return nil, errors.New("capturemix inert connection spy")
			})
			if fixture != nil || err == nil || err.Error() != "capturemix cleanup database target refused" || calls != 0 {
				t.Fatal("capturemix cleanup fixture bypassed harness validation")
			}
		})
	}
	t.Run("lane_guard", func(t *testing.T) {
		t.Setenv("WITSELF_SYNC_HARNESS_DB", "codex_b")
		calls := 0
		fixture, err := openCleanupTestCluster(context.Background(), cleanupTargetURL("127.0.0.1:5599", "gate_a"), func(context.Context, string) (*pgx.Conn, error) {
			calls++
			return nil, errors.New("capturemix inert connection spy")
		})
		if fixture != nil || err == nil || err.Error() != "capturemix cleanup database target refused" || calls != 0 {
			t.Fatal("capturemix cleanup fixture bypassed the database lane guard")
		}
	})
	if !child && !t.Failed() {
		runCleanupTargetProductionOverlay(t)
	}
}

func cleanupTargetURL(host, database string) string {
	return (&url.URL{Scheme: "postgres", Host: host, User: url.UserPassword("postgres", "test"), Path: "/" + database, RawQuery: "sslmode=disable"}).String()
}

func cleanupGoRestTarget(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal("capturemix cleanup CI workflow unavailable")
	}
	var document yaml.Node
	if yaml.Unmarshal(raw, &document) != nil || document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		t.Fatal("capturemix cleanup CI workflow shape changed")
	}
	jobs := cleanupYAMLField(t, document.Content[0], "jobs", yaml.MappingNode)
	job := cleanupYAMLField(t, jobs, "go-rest", yaml.MappingNode)
	steps := cleanupYAMLField(t, job, "steps", yaml.SequenceNode)
	var testStep *yaml.Node
	for _, step := range steps.Content {
		if step.Kind != yaml.MappingNode || len(step.Content)%2 != 0 {
			t.Fatal("capturemix cleanup CI step shape changed")
		}
		for i := 0; i < len(step.Content); i += 2 {
			if step.Content[i].Value == "name" && step.Content[i+1].Value == "test non-store" {
				if testStep != nil {
					t.Fatal("capturemix cleanup CI test step is ambiguous")
				}
				testStep = step
			}
		}
	}
	if testStep == nil {
		t.Fatal("capturemix cleanup CI test step missing")
	}
	if cleanupYAMLField(t, testStep, "name", yaml.ScalarNode).Tag != "!!str" || cleanupYAMLField(t, testStep, "shell", yaml.ScalarNode).Value != "bash" {
		t.Fatal("capturemix cleanup CI test step shape changed")
	}
	variables := cleanupYAMLField(t, testStep, "env", yaml.MappingNode)
	requirement := cleanupYAMLField(t, variables, "WITSELF_TEST_REQUIRE_DATABASE", yaml.ScalarNode)
	target := cleanupYAMLField(t, variables, "WITSELF_TEST_DATABASE_URL", yaml.ScalarNode)
	if requirement.Tag != "!!str" || requirement.Value != "1" || target.Tag != "!!str" || target.Value == "" || strings.Contains(target.Value, "${{") {
		t.Fatal("capturemix cleanup CI database requirement changed")
	}
	return target.Value
}

func cleanupYAMLField(t *testing.T, mapping *yaml.Node, name string, kind yaml.Kind) *yaml.Node {
	t.Helper()
	if mapping.Kind != yaml.MappingNode || len(mapping.Content)%2 != 0 {
		t.Fatal("capturemix cleanup CI mapping shape changed")
	}
	var found *yaml.Node
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == name {
			if found != nil || mapping.Content[i].Kind != yaml.ScalarNode || mapping.Content[i].Tag != "!!str" {
				t.Fatal("capturemix cleanup CI mapping is ambiguous")
			}
			found = mapping.Content[i+1]
		}
	}
	if found == nil || found.Kind != kind {
		t.Fatal("capturemix cleanup CI required field shape changed")
	}
	return found
}

func assertCleanupTargetSourceBoundary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal("capturemix cleanup production sources unavailable")
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		source, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, 0)
		if err != nil {
			t.Fatal("capturemix cleanup production source invalid")
		}
		ast.Inspect(source, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if ok && (identifier.Name == "openCleanupTestCluster" || strings.HasPrefix(identifier.Name, "cleanupTarget")) {
				t.Error("capturemix production source references the test-only cleanup constructor or spy")
			}
			return true
		})
	}
	source, err := parser.ParseFile(token.NewFileSet(), "cleanup_test.go", nil, 0)
	if err != nil {
		t.Fatal("capturemix cleanup fixture source unavailable")
	}
	calls := 0
	for _, declaration := range source.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "cleanupTestCluster" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, ok := call.Fun.(*ast.Ident)
			if !ok || name.Name != "openCleanupTestCluster" {
				return true
			}
			calls++
			if len(call.Args) != 3 {
				t.Fatal("capturemix cleanup fixture constructor shape changed")
			}
			connect, ok := call.Args[2].(*ast.SelectorExpr)
			if !ok || connect.Sel.Name != "Connect" {
				t.Fatal("capturemix cleanup fixture connection changed")
			}
			pkg, ok := connect.X.(*ast.Ident)
			if !ok || pkg.Name != "pgx" {
				t.Fatal("capturemix cleanup fixture connection changed")
			}
			return true
		})
	}
	if calls != 1 {
		t.Fatal("capturemix cleanup fixture must use its test-only constructor exactly once")
	}
}

func runCleanupTargetProductionOverlay(t *testing.T) {
	t.Helper()
	directory, err := filepath.Abs(".")
	if err != nil {
		t.Fatal("capturemix cleanup source location unavailable")
	}
	staging := t.TempDir()
	replacements := map[string]string{}
	for _, item := range []struct{ name, before, after string }{
		{"postgres.go", "admin, err := pgx.Connect(ctx, dsn)", "admin, err := cleanupTargetProductionConnect(ctx, dsn)"},
		{"cleanup_target_test.go", "const cleanupTargetOverlayActive = " + "false", "const cleanupTargetOverlayActive = " + "true"},
	} {
		source := filepath.Join(directory, item.name)
		raw, err := os.ReadFile(source)
		if err != nil {
			t.Fatal("capturemix cleanup overlay source unavailable")
		}
		// Source contracts must work with both LF and Windows CRLF checkouts.
		raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
		if bytes.Count(raw, []byte(item.before)) != 1 {
			t.Fatal("capturemix cleanup overlay connection site changed")
		}
		overlay := filepath.Join(staging, item.name)
		if os.WriteFile(overlay, bytes.Replace(raw, []byte(item.before), []byte(item.after), 1), 0600) != nil {
			t.Fatal("capturemix cleanup overlay source write failed")
		}
		replacements[source] = overlay
	}
	raw, err := json.Marshal(struct{ Replace map[string]string }{Replace: replacements})
	if err != nil {
		t.Fatal("capturemix cleanup overlay encoding failed")
	}
	overlay := filepath.Join(staging, "overlay.json")
	if os.WriteFile(overlay, raw, 0600) != nil {
		t.Fatal("capturemix cleanup overlay write failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-overlay", overlay, "-run", "^TestCaptureMixCleanupTargetsHermetic$", "-count=1", "-timeout=90s", ".")
	command.Dir = directory
	command.Env = append(os.Environ(), cleanupTargetChildEnv+"=1", "GOTOOLCHAIN=local", "GOPROXY=off")
	output, err := command.CombinedOutput()
	if err != nil {
		// Never forward build or test output: it may contain target-bearing source
		// excerpts. The fixed child diagnostics identify the assertion safely.
		for _, diagnostic := range []string{
			"capturemix cleanup target production allowlist changed",
			"capturemix production connection boundary changed",
		} {
			if bytes.Contains(output, []byte(diagnostic)) {
				t.Log(diagnostic)
			}
		}
		t.Fatal("capturemix cleanup production connection overlay failed")
	}
	t.Log("capturemix cleanup production target spy passed without database connections")
}
