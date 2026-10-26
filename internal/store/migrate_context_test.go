package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestMigrationWaitPhase(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, phase := range []string{"advisory lock", "preflight", "verify"} {
		err := migrationContextError(ctx, phase)
		if !errors.Is(err, ErrMigrationWaitCanceled) || err.Error() != "migration wait canceled: "+phase {
			t.Fatalf("cancellation = %v, want value-free sentinel for %s", err, phase)
		}
		if got := MigrationWaitPhase(fmt.Errorf("outer: %w", err)); got != phase {
			t.Fatalf("wrapped phase = %q, want %q", got, phase)
		}
	}
	if MigrationWaitPhase(errors.New("ordinary failure")) != "" || MigrationWaitPhase(nil) != "" {
		t.Fatal("ordinary failure has a migration cancellation phase")
	}
	if err := migrationContextError(context.Background(), "verify"); err != nil {
		t.Fatalf("background context: %v", err)
	}
}
