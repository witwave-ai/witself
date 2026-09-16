package transcriptcapture

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// All successful claimants retain ownership until every contender has
// returned. More than one success therefore proves overlapping ownership.
func TestFlushLockConcurrentStaleRecoveryHasOneOwner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, "witself"))
	t.Setenv("DSH_HOME", filepath.Join(home, "dsh"))
	dir, err := outboxDir(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if running, known := processRunning(2147483647); !known || running {
		t.Skip("fixture dead PID is unavailable")
	}
	for attempt := 0; attempt < 100; attempt++ {
		if err := os.WriteFile(filepath.Join(dir, ".flush.lock"), []byte("2147483647\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		const contenders = 32
		type result struct {
			release func()
			owned   bool
			err     error
		}
		results := make(chan result, contenders)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < contenders; i++ {
			wg.Go(func() {
				<-start
				release, owned, err := AcquireFlushLock(RuntimeClaudeCode)
				results <- result{release, owned, err}
			})
		}
		close(start)
		wg.Wait()
		close(results)
		owners := 0
		for result := range results {
			if result.owned {
				owners++
			}
			if result.release != nil {
				result.release()
			}
			if result.err != nil {
				t.Error(result.err)
			}
		}
		if owners != 1 {
			t.Fatalf("attempt %d: %d simultaneous owners, want exactly one", attempt, owners)
		}
	}
}
