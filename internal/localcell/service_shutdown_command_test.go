package localcell_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Command tests live in testdata to keep this slice inside its production-file
// allowlist. A Go overlay compiles them in each real command package: no copied
// command, fake runtime, store, or changed working-tree file is involved.
func TestServiceShutdownCommandProductionContexts(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	// Compile/subprocess hang guard only; behavioural watchdogs scale with fixture timing.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	for _, role := range []string{"server", "worker"} {
		t.Run(role, func(t *testing.T) {
			replacements := map[string]string{
				filepath.Join(root, "internal", "localcell", "service_shutdown_fixture_overlay.go"): filepath.Join(root, "internal", "localcell", "testdata", "shutdown-command", "protocol.go"),
				filepath.Join(root, "cmd", "witself-"+role, "service_shutdown_overlay_test.go"):     filepath.Join(root, "internal", "localcell", "testdata", "shutdown-command", role+"_test.go"),
			}
			body, err := json.Marshal(struct{ Replace map[string]string }{replacements})
			if err != nil {
				t.Fatal(err)
			}
			overlay := filepath.Join(t.TempDir(), "overlay.json")
			if err := os.WriteFile(overlay, body, 0o600); err != nil {
				t.Fatal(err)
			}
			// The 20-second child limit is a subprocess hang guard, not a latency assertion.
			cmd := exec.CommandContext(ctx, "go", "test", "-overlay", overlay, "./cmd/witself-"+role, "-run", "^TestCommandShutdown", "-count=1", "-timeout=20s")
			cmd.Dir = root
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Run(); err != nil {
				t.Fatalf("%s command shutdown proof failed:\n%s", role, sanitizeNativeServiceDiagnostic(output.String(), root, nativeServiceDiagnosticHome()))
			}
			if !strings.Contains(output.String(), "ok  ") || strings.Contains(output.String(), "no tests to run") {
				t.Fatal("command proof did not execute its tests")
			}
		})
	}
}

func TestServiceShutdownCommandWiring(t *testing.T) {
	for _, role := range []string{"server", "worker"} {
		t.Run(role, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("..", "..", "cmd", "witself-"+role, "main.go"))
			if err != nil {
				t.Fatal(err)
			}
			source := strings.ReplaceAll(string(body), "\r\n", "\n")
			install := strings.Index(source, "ctx, shutdownChannel, closeShutdown, err := installCommandShutdown(ctx, runtime)")
			openStore := strings.Index(source, "store.Open(ctx, dsn")
			if install < 0 || openStore < install || !strings.Contains(source, "defer closeShutdown()") {
				t.Fatal("command no longer waits at the shutdown release barrier before store startup")
			}
			if role == "server" && (!strings.Contains(source, "cfg = serverConfigWithShutdown(cfg, shutdownChannel)") || !strings.Contains(source, "startup.run(ctx, cfg)")) {
				t.Fatal("server bypasses the proven cancellation context path")
			}
			if role == "worker" && !strings.Contains(source, "runWorkerWithShutdown(runtimeCtx, registry, cfg, shutdownChannel)") {
				t.Fatal("worker bypasses the proven cancellation context path")
			}
		})
	}
}
