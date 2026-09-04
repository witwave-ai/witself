package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const helperModeEnv = "WITSELF_TESTENV_HELPER_MODE"

func TestRequirePostgres(t *testing.T) {
	t.Run("returns configured DSN", func(t *testing.T) {
		t.Setenv(databaseURLEnv, "postgres://configured")
		t.Setenv(requireDatabaseEnv, "1")
		if got := RequirePostgres(t); got != "postgres://configured" {
			t.Fatalf("RequirePostgres() = %q", got)
		}
	})

	t.Run("missing DSN skips", func(t *testing.T) {
		t.Setenv(databaseURLEnv, "")
		t.Setenv(requireDatabaseEnv, "")
		t.Setenv(helperModeEnv, "postgres")
		output, err := runHelperProcess(t)
		if err != nil {
			t.Fatalf("skip helper failed: %v\n%s", err, output)
		}
		if !strings.Contains(output, "WITSELF_TEST_DATABASE_URL is not set") ||
			!strings.Contains(output, "--- SKIP: TestRequirementHelperProcess/postgres") {
			t.Fatalf("helper did not skip with the standard message:\n%s", output)
		}
	})

	t.Run("only exact opt-in requires DSN", func(t *testing.T) {
		t.Setenv(databaseURLEnv, "")
		t.Setenv(requireDatabaseEnv, "true")
		t.Setenv(helperModeEnv, "postgres")
		output, err := runHelperProcess(t)
		if err != nil {
			t.Fatalf("non-exact opt-in helper failed: %v\n%s", err, output)
		}
		if !strings.Contains(output, "--- SKIP: TestRequirementHelperProcess/postgres") {
			t.Fatalf("non-exact opt-in did not skip:\n%s", output)
		}
	})

	t.Run("missing required DSN fails", func(t *testing.T) {
		t.Setenv(databaseURLEnv, "")
		t.Setenv(requireDatabaseEnv, "1")
		t.Setenv(helperModeEnv, "postgres")
		output, err := runHelperProcess(t)
		if err == nil {
			t.Fatalf("required helper unexpectedly passed:\n%s", output)
		}
		if !strings.Contains(output, "WITSELF_TEST_DATABASE_URL is not set (WITSELF_TEST_REQUIRE_DATABASE=1)") ||
			!strings.Contains(output, "--- FAIL: TestRequirementHelperProcess/postgres") {
			t.Fatalf("helper did not fail with the requirement message:\n%s", output)
		}
	})
}

func TestRequireNode(t *testing.T) {
	t.Run("returns resolved executable", func(t *testing.T) {
		dir := t.TempDir()
		name := "node"
		if runtime.GOOS == "windows" {
			name += ".exe"
			t.Setenv("PATHEXT", ".EXE")
		}
		node := filepath.Join(dir, name)
		if err := os.WriteFile(node, nil, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		t.Setenv(requireNodeEnv, "1")
		if got := RequireNode(t); got != node {
			t.Fatalf("RequireNode() = %q, want %q", got, node)
		}
	})

	t.Run("missing Node skips", func(t *testing.T) {
		t.Setenv("PATH", "")
		t.Setenv(requireNodeEnv, "")
		t.Setenv(helperModeEnv, "node")
		output, err := runHelperProcess(t)
		if err != nil {
			t.Fatalf("skip helper failed: %v\n%s", err, output)
		}
		if !strings.Contains(output, "node is not installed") ||
			!strings.Contains(output, "--- SKIP: TestRequirementHelperProcess/node") {
			t.Fatalf("helper did not skip with the standard message:\n%s", output)
		}
	})

	t.Run("missing required Node fails", func(t *testing.T) {
		t.Setenv("PATH", "")
		t.Setenv(requireNodeEnv, "1")
		t.Setenv(helperModeEnv, "node")
		output, err := runHelperProcess(t)
		if err == nil {
			t.Fatalf("required helper unexpectedly passed:\n%s", output)
		}
		if !strings.Contains(output, "node is not installed (WITSELF_TEST_REQUIRE_NODE=1)") ||
			!strings.Contains(output, "--- FAIL: TestRequirementHelperProcess/node") {
			t.Fatalf("helper did not fail with the requirement message:\n%s", output)
		}
	})
}

func TestRequirementHelperProcess(t *testing.T) {
	switch os.Getenv(helperModeEnv) {
	case "postgres":
		t.Run("postgres", func(t *testing.T) {
			RequirePostgres(t)
		})
	case "node":
		t.Run("node", func(t *testing.T) {
			RequireNode(t)
		})
	}
}

func runHelperProcess(t *testing.T) (string, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable,
		"-test.run=^TestRequirementHelperProcess$",
		"-test.v",
		"-test.count=1",
	)
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	return string(output), err
}
