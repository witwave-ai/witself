package localcell_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestServiceShutdownHostedHelperArguments exercises built command binaries:
// package initialization happens before command dispatch and cannot be covered
// by calling the commands' run functions in an overlay alone.
func TestServiceShutdownHostedHelperArguments(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	// Compilation and subprocess hang guard, not a behavioral timing assertion.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	for _, role := range []string{"server", "worker"} {
		t.Run(role, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "witself-"+role)
			if runtime.GOOS == "windows" {
				binary += ".exe"
			}
			build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/witself-"+role)
			build.Dir = root
			if output, err := build.CombinedOutput(); err != nil {
				t.Fatalf("command compilation failed: %s", sanitizeNativeServiceDiagnostic(string(output), root, nativeServiceDiagnosticHome()))
			}
			run := func(args ...string) ([]byte, []byte, int) {
				t.Helper()
				cmd := exec.CommandContext(ctx, binary, args...)
				// No local selector or inherited credentials enter these subprocesses.
				cmd.Env = []string{"WITSELF_HOME=" + t.TempDir(), "DSH_HOME=" + t.TempDir()}
				if runtime.GOOS == "windows" {
					cmd.Env = append(cmd.Env, "SystemRoot="+os.Getenv("SystemRoot"))
				}
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err := cmd.Run()
				code := 0
				if err != nil {
					var exit *exec.ExitError
					if !errors.As(err, &exit) {
						t.Fatal("hosted command did not complete")
					}
					code = exit.ExitCode()
				}
				return stdout.Bytes(), stderr.Bytes(), code
			}
			help, helpErr, code := run("--help")
			if code != 0 || len(helpErr) != 0 || !bytes.Contains(help, []byte("Usage:")) {
				t.Fatal("hosted usage baseline unavailable")
			}
			for _, args := range [][]string{
				{"--localcell-internal-exec", t.TempDir(), "invalid-component", "unused-binary"},
				{"--localcell-internal-stop", t.TempDir(), "invalid-component", "0", "unused-identity"},
			} {
				stdout, stderr, code := run(args...)
				want := append([]byte(fmt.Sprintf("witself-%s: unknown command %q\n\n", role, args[0])), help...)
				if code != 2 || len(stdout) != 0 || !bytes.Equal(stderr, want) {
					t.Fatalf("hosted helper-shaped arguments bypassed ordinary command usage: exit=%d, want exit=2 and byte-identical unknown-command usage", code)
				}
			}
		})
	}
}
