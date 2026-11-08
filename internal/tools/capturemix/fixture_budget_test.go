package capturemix

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Fixture construction includes the entire migration set. Reserve one cleanup
// allowance at the test binary's deadline instead of spending the drop budget
// on construction. An absent or exhausted deadline still gets a bounded budget.
func newFixtureConstructionContext(deadline func() (time.Time, bool)) (context.Context, context.CancelFunc) {
	now := time.Now()
	constructionDeadline := now.Add(workerOutcomeBudgetCap)
	if testDeadline, ok := deadline(); ok {
		reserved := testDeadline.Add(-proofWorkerInterval("MESSAGE_RATE_BUCKET_CLEANUP") / 2)
		if reserved.After(now) {
			constructionDeadline = reserved
		}
	}
	return context.WithDeadline(context.Background(), constructionDeadline)
}

func TestCaptureMixFixtureConstructionBudget(t *testing.T) {
	now := time.Now()
	reserve := proofWorkerInterval("MESSAGE_RATE_BUCKET_CLEANUP") / 2
	for _, test := range []struct {
		name     string
		deadline time.Time
		present  bool
		fallback bool
	}{
		{name: "derived_deadline", deadline: now.Add(3 * workerOutcomeBudgetCap), present: true},
		{name: "reserve", deadline: now.Add(reserve + workerOutcomeBudgetCap/2), present: true},
		{name: "absent_deadline", fallback: true},
		{name: "short_deadline", deadline: now.Add(reserve / 2), present: true, fallback: true},
		{name: "exhausted_reserve", deadline: now.Add(reserve), present: true, fallback: true},
		{name: "expired_deadline", deadline: now.Add(-reserve), present: true, fallback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := time.Now()
			ctx, cancel := newFixtureConstructionContext(func() (time.Time, bool) {
				return test.deadline, test.present
			})
			defer cancel()
			after := time.Now()
			got, bounded := ctx.Deadline()
			if !bounded || ctx.Err() != nil {
				t.Fatal("capturemix fixture construction must have a live bounded context")
			}
			if test.fallback {
				if got.Before(before.Add(workerOutcomeBudgetCap)) || got.After(after.Add(workerOutcomeBudgetCap)) {
					t.Fatal("capturemix fixture construction fallback must use the worker outcome cap")
				}
			} else if !got.Equal(test.deadline.Add(-reserve)) {
				t.Fatal("capturemix fixture construction must reserve cleanup at the test deadline")
			}
			cancel()
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal("capturemix fixture construction context must release on cancellation")
			}
		})
	}
}

// Allocation cleanup can join process metadata into its error. Return only a
// closed construction/cleanup class, never the original error or its suffix.
func fixtureConstructionErrorClass(err error) string {
	for _, class := range []string{
		"capturemix random input failed",
		"capturemix random identity failed",
		"capturemix database allocation failed",
		"capturemix schema allocation failed",
		"capturemix fixture connection failed",
		"capturemix initial schema isolation failed",
		"capturemix fixture store failed",
		"capturemix core migration failed",
		"capturemix migrated schema isolation failed",
		"capturemix core coordinate mismatch",
		"capturemix migration connection failed",
		"capturemix migration source failed",
		"capturemix migration provider failed",
		"capturemix version one migration failed",
		"capturemix initial schema mismatch",
		"capturemix sync selection failed",
		"capturemix fixture seed failed",
		"capturemix fixture credential failed",
		"capturemix startup grant failed",
		"capturemix marker installation failed",
		"capturemix allocation cleanup failed",
		"capturemix process cleanup failed",
	} {
		if strings.Contains(err.Error(), class) {
			return class
		}
	}
	return "capturemix fixture failure unclassified"
}

func TestCaptureMixFixtureConstructionDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"migration", errors.New("capturemix core migration failed"), "capturemix core migration failed"},
		{"seed", errors.New("capturemix fixture seed failed: accounts sqlstate=23505"), "capturemix fixture seed failed"},
		{"joined_cleanup", errors.Join(errors.New("capturemix fixture connection failed"), errors.New("capturemix allocation cleanup failed"), errors.New("private process metadata")), "capturemix fixture connection failed"},
		{"process_metadata", errors.New("capturemix process cleanup failed: private process metadata"), "capturemix process cleanup failed"},
		{"unknown", errors.New("private database detail"), "capturemix fixture failure unclassified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if fixtureConstructionErrorClass(test.err) != test.want {
				t.Fatal("capturemix fixture construction diagnostic lost its value-free class")
			}
		})
	}
}

func TestCaptureMixFixtureConstructionFailureContract(t *testing.T) {
	raw, err := os.ReadFile("cleanup_test.go")
	if err != nil {
		t.Fatal("capturemix fixture construction test source unavailable")
	}
	// The multiline source contract must also match Windows checkouts.
	source := strings.ReplaceAll(string(raw), "\r\n", "\n")
	_, body, found := strings.Cut(source, "func TestCaptureMixFailedWriterCleanupAllowsNextFixture(t *testing.T) {")
	if !found {
		t.Fatal("capturemix failed-writer cleanup test missing")
	}
	body, _, _ = strings.Cut(body, "\nfunc ")
	const construction = `constructionContext, cancelConstruction := newFixtureConstructionContext(t.Deadline)
	defer cancelConstruction()
	constructionStarted := time.Now()
	nextFixture, err := nextCluster.fixture(constructionContext, Options{}, "old", "new")
	if err != nil {
		t.Fatalf("capturemix next fixture failed after writer failure cleanup: %s (construction elapsed %s)", fixtureConstructionErrorClass(err), time.Since(constructionStarted))
	}`
	if !strings.Contains(body, construction) {
		t.Fatal("capturemix next fixture must use its construction budget and report error class with elapsed time")
	}
}
