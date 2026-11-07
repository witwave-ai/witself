package capturemix

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestOrchestrationFixtureCleanupAfterWriterFailure(t *testing.T) {
	backend := &fakeRow{fail: "writers", stopFail: true}
	cleanups := 0
	checks, err := executeRowWithCleanup(context.Background(), backend, "commit", func() error {
		cleanups++
		if backend.steps[len(backend.steps)-1] != "stop" {
			t.Fatal("capturemix fixture cleanup preceded process cleanup")
		}
		return errors.New("capturemix injected fixture cleanup failure")
	})
	if cleanups != 1 || len(checks) != 7 || err == nil {
		t.Fatal("capturemix writer failure omitted fixture cleanup")
	}
	for _, expected := range []string{
		"capturemix stage failed: writer_assertions",
		"capturemix injected stage failure",
		"capturemix injected stop failure",
		"capturemix fixture cleanup failed",
		"capturemix injected fixture cleanup failure",
	} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatal("capturemix fixture cleanup concealed a preceding failure")
		}
	}
}

func TestCaptureMixCleanupContextHasIndependentProportionalBudget(t *testing.T) {
	before := time.Now()
	first, cancelFirst := newCleanupContext()
	cancelFirst()
	second, cancelSecond := newCleanupContext()
	defer cancelSecond()
	after := time.Now()
	deadline, ok := second.Deadline()
	budget := proofWorkerInterval("MESSAGE_RATE_BUCKET_CLEANUP") / 2
	if first.Err() == nil || second.Err() != nil || !ok ||
		deadline.Before(before.Add(budget)) || deadline.After(after.Add(budget)) {
		t.Fatal("capturemix cleanup contexts share cancellation or lose the configured interval bound")
	}
}

// Keep failure injection in the row backend while exercising real catalog
// cleanup. Neither the failed fixture connection, its closed admin, nor the
// canceled row context can be reused to remove this schema.
func TestCaptureMixFailedWriterCleanupAllowsNextFixture(t *testing.T) {
	c := cleanupTestCluster(t)
	ctx, cancel := newCleanupContext()
	defer cancel()
	suffix, err := randomName()
	if err != nil {
		t.Fatal("capturemix cleanup test identity unavailable")
	}
	name := "capturemix_dry_" + suffix
	t.Cleanup(func() {
		if err := c.cleanupExec(`DROP SCHEMA IF EXISTS `+pgx.Identifier{name}.Sanitize()+` CASCADE`, "capturemix cleanup test schema recovery failed"); err != nil {
			t.Error(err)
		}
	})
	if _, err := c.admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal("capturemix cleanup test schema allocation failed")
	}
	db, err := pgx.Connect(ctx, c.dsn)
	if err != nil {
		t.Fatal("capturemix cleanup test fixture connection failed")
	}
	f := &fixture{cluster: c, name: name, db: db, allocated: true}
	if db.Close(ctx) != nil || c.admin.Close(ctx) != nil {
		t.Fatal("capturemix cleanup test connection failure injection failed")
	}
	rowContext, cancelRow := context.WithCancel(context.Background())
	cancelRow()
	_, err = executeRowWithCleanup(rowContext, &fakeRow{fail: "writers"}, "commit", f.close)
	if err == nil || !strings.Contains(err.Error(), "capturemix stage failed: writer_assertions") ||
		strings.Contains(err.Error(), "capturemix fixture cleanup failed") {
		t.Fatal("capturemix failed writer did not retain its failure and complete fixture cleanup")
	}

	nextCluster := cleanupTestCluster(t)
	var exists bool
	if nextCluster.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, name).Scan(&exists) != nil || exists {
		t.Fatal("capturemix failed writer left its fixture schema")
	}
	nextFixture, err := nextCluster.fixture(ctx, Options{}, "old", "new")
	if err != nil {
		t.Fatal("capturemix next fixture failed after writer failure cleanup")
	}
	if err := nextFixture.close(); err != nil {
		t.Error(err)
	}
}

func TestCaptureMixClusterCleanupContinuesAfterRoleFailure(t *testing.T) {
	c := cleanupTestCluster(t)
	ctx, cancel := newCleanupContext()
	defer cancel()
	suffix, err := randomName()
	if err != nil {
		t.Fatal("capturemix role cleanup test identity unavailable")
	}
	roles := []string{"capturemix_first_dry_" + suffix, "capturemix_last_dry_" + suffix}
	for _, role := range roles {
		createCleanupTestRole(t, c, role)
	}
	// Drops run in reverse order. An absent role injects a catalog failure
	// between two real per-test roles; neither successful drop may be skipped.
	c.created = []string{roles[0], "capturemix_absent_dry_" + suffix, roles[1]}
	if c.admin.Close(ctx) != nil {
		t.Fatal("capturemix role cleanup closed-admin injection failed")
	}
	err = c.close()
	if err == nil || err.Error() != "capturemix role cleanup failed" {
		t.Fatal("capturemix role cleanup lost its fixed failure diagnostic")
	}
	observer := cleanupTestCluster(t)
	var count int
	if observer.admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, roles).Scan(&count) != nil || count != 0 {
		t.Fatal("capturemix role cleanup failure stranded another per-test role")
	}
}

// Register recovery before CREATE, including the ambiguous-acknowledgment path.
// Each registration is independent, so Fatal while allocating the next role
// still removes every earlier role using a fresh connection and context.
func createCleanupTestRole(t *testing.T, c *cluster, role string) {
	t.Helper()
	t.Cleanup(func() {
		if err := c.cleanupExec(`DROP ROLE IF EXISTS `+pgx.Identifier{role}.Sanitize(), "capturemix role cleanup test recovery failed"); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := newCleanupContext()
	defer cancel()
	if _, err := c.admin.Exec(ctx, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` NOLOGIN`); err != nil {
		t.Fatal("capturemix role cleanup test allocation failed")
	}
}

func TestCaptureMixRoleAllocationFatalCleansEarlierRole(t *testing.T) {
	const childRole = "WITSELF_CAPTUREMIX_CLEANUP_FATAL_ROLE"
	const prefix = "capturemix_fatal_dry_"
	if role := os.Getenv(childRole); role != "" {
		if !strings.HasPrefix(role, prefix) || len(role) <= len(prefix) || len(role) > 63 ||
			strings.Trim(role, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
			t.Fatal("capturemix fatal cleanup role identity refused")
		}
		c := cleanupTestCluster(t)
		createCleanupTestRole(t, c, role)
		t.Fatal("capturemix injected fatal after first role allocation")
	}

	c := cleanupTestCluster(t)
	suffix, err := randomName()
	if err != nil {
		t.Fatal("capturemix fatal cleanup identity unavailable")
	}
	role := prefix + suffix
	// Parent recovery keeps even a failing mutation from polluting the lane.
	// The absence assertion below runs before this fallback and must still fail.
	t.Cleanup(func() {
		if err := c.cleanupExec(`DROP ROLE IF EXISTS `+pgx.Identifier{role}.Sanitize(), "capturemix fatal cleanup parent recovery failed"); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := newCleanupContext()
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCaptureMixRoleAllocationFatalCleansEarlierRole$", "-test.v")
	command.Env = append(os.Environ(), childRole+"="+role)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !expectedCleanupFatalOutput(output) {
		t.Fatal("capturemix fatal cleanup child did not reach only the expected allocation failure")
	}
	var count int
	if c.admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=$1`, role).Scan(&count) != nil || count != 0 {
		t.Fatal("capturemix fatal allocation left per-test role residue")
	}
}

func expectedCleanupFatalOutput(output []byte) bool {
	const name = "TestCaptureMixRoleAllocationFatalCleansEarlierRole"
	// A self-executed go-rest test binary retains its coverage instrumentation.
	// Permit that numeric summary, while rejecting every extra error diagnostic.
	pattern := `\A=== RUN   ` + name + `\r?\n    cleanup_test.go:[0-9]+: capturemix injected fatal after first role allocation\r?\n--- FAIL: ` + name + ` \([0-9.]+s\)\r?\nFAIL\r?\n(?:coverage: [0-9]+\.[0-9]+% of statements\r?\n)?\z`
	return regexp.MustCompile(pattern).Match(output)
}

func TestCaptureMixFatalChildOutputRejectsCleanupErrors(t *testing.T) {
	const name = "TestCaptureMixRoleAllocationFatalCleansEarlierRole"
	output := "=== RUN   " + name + "\n    cleanup_test.go:1: capturemix injected fatal after first role allocation\n--- FAIL: " + name + " (0.01s)\nFAIL\n"
	for _, summary := range []string{"", "coverage: 0.0% of statements\n", "coverage: 100.0% of statements\n"} {
		for _, newline := range []string{"\n", "\r\n"} {
			if !expectedCleanupFatalOutput([]byte(strings.ReplaceAll(output+summary, "\n", newline))) {
				t.Fatal("capturemix expected child failure output refused")
			}
		}
	}
	for _, diagnostic := range []string{
		"    cleanup_test.go:1: capturemix role cleanup test recovery failed\n",
		"    cleanup_test.go:1: postgres transaction guard observation failed\n",
		"WARNING: DATA RACE\n",
		"coverage: unexpected diagnostic\n",
	} {
		if expectedCleanupFatalOutput([]byte(strings.Replace(output, "--- FAIL:", diagnostic+"--- FAIL:", 1))) ||
			expectedCleanupFatalOutput([]byte(output+"coverage: 1.1% of statements\n"+diagnostic)) {
			t.Fatal("capturemix expected child failure concealed another diagnostic")
		}
	}
}

func cleanupTestCluster(t *testing.T) *cluster {
	t.Helper()
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := newCleanupContext()
	defer cancel()
	c, err := openCleanupTestCluster(ctx, dsn, pgx.Connect)
	if err != nil {
		t.Fatal("capturemix cleanup test cluster unavailable")
	}
	t.Cleanup(func() {
		ctx, cancel := newCleanupContext()
		defer cancel()
		if c.admin.Close(ctx) != nil {
			t.Error("capturemix cleanup test admin close failed")
		}
	})
	return c
}
