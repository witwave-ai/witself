// Package testenv centralizes opt-in test prerequisites.
package testenv

import (
	"os"
	"os/exec"
	"testing"
)

const (
	databaseURLEnv     = "WITSELF_TEST_DATABASE_URL"
	requireDatabaseEnv = "WITSELF_TEST_REQUIRE_DATABASE"
	requireNodeEnv     = "WITSELF_TEST_REQUIRE_NODE"
)

// RequirePostgres returns the configured PostgreSQL test DSN. When the DSN is
// absent, it skips by default and fails if WITSELF_TEST_REQUIRE_DATABASE=1.
func RequirePostgres(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(databaseURLEnv)
	if dsn != "" {
		return dsn
	}
	if os.Getenv(requireDatabaseEnv) == "1" {
		t.Fatal("WITSELF_TEST_DATABASE_URL is not set (WITSELF_TEST_REQUIRE_DATABASE=1)")
		return ""
	}
	t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	return ""
}

// RequireNode returns the resolved Node executable. When Node is absent, it
// skips by default and fails if WITSELF_TEST_REQUIRE_NODE=1.
func RequireNode(t testing.TB) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err == nil {
		return node
	}
	if os.Getenv(requireNodeEnv) == "1" {
		t.Fatal("node is not installed (WITSELF_TEST_REQUIRE_NODE=1)")
		return ""
	}
	t.Skip("node is not installed")
	return ""
}
