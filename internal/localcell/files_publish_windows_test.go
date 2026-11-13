//go:build windows

package localcell

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// A stopped foreground host runs Stop on its first start, and Stop publishes
// manifest.json (finish) through the ordinary writeAtomic. Meanwhile the
// readiness client validates the whole private tree and holds the validator's
// guard on each object it checks. Hold that exact guard on the published
// manifest or on Stop's pending source while the real Stop runs, and release it
// only at a fixed point: a counted attempt's source sync. No sleep synchronizes
// this proof. A host-runnable contract pins the guard reuse.
func TestWindowsStopManifestPublicationValidatorGuard(t *testing.T) {
	t.Run("single_attempt_published_manifest", func(t *testing.T) {
		c, m := unitCell(t)
		path := c.path("manifest.json")
		before := windowsBootPublishState(t, path)
		guard := holdWindowsBootValidatorGuard(t, path)
		// The manifest write Stop made before this slice was one such attempt.
		m.LastOperation = "stop"
		err := writeAtomicOnce(path, m, nil)
		guard.release()
		windowsManifestSharingRefusal(t, err, "validator guard on the manifest did not refuse a single publication with a sharing violation")
		windowsManifestUnchanged(t, c, before)
	})

	t.Run("single_attempt_pending_source", func(t *testing.T) {
		c, m := unitCell(t)
		path := c.path("manifest.json")
		before := windowsBootPublishState(t, path)
		var guard *windowsBootValidatorGuard
		attempts := observeWindowsManifestPublications(t, c, func(attempt int, pending string) {
			if attempt == 1 {
				guard = holdWindowsBootValidatorGuard(t, pending)
			}
		})
		m.LastOperation = "stop"
		err := writeAtomicOnce(path, m, nil)
		if guard == nil || *attempts != 1 {
			t.Fatal("single manifest publication did not reach its pending source once")
		}
		guard.release()
		windowsManifestSharingRefusal(t, err, "validator guard on the pending source did not refuse a single manifest publication with a sharing violation")
		windowsManifestUnchanged(t, c, before)
	})

	t.Run("retry_published_manifest_released", func(t *testing.T) {
		c, _ := unitCell(t)
		path := c.path("manifest.json")
		before := windowsBootPublishState(t, path)
		guard := holdWindowsBootValidatorGuard(t, path)
		attempts := observeWindowsManifestPublications(t, c, func(attempt int, _ string) {
			if attempt == 2 {
				guard.release()
			}
		})
		err := c.stopServiceChildren(t.Context())
		guard.release()
		if err != nil {
			t.Fatal("Stop did not survive a released validator guard on the manifest")
		}
		// The guard refuses the first attempt; the second runs after release. A
		// foreign handle on a CI runner may add attempts, never remove one.
		if *attempts < 2 {
			t.Fatal("Stop did not retry its manifest publication around the released guard")
		}
		if os.SameFile(before.info, windowsBootPublishState(t, path).info) {
			t.Fatal("Stop did not replace the guarded manifest")
		}
		windowsManifestStopped(t, c)
	})

	t.Run("retry_pending_source_released", func(t *testing.T) {
		c, _ := unitCell(t)
		var guard *windowsBootValidatorGuard
		attempts := observeWindowsManifestPublications(t, c, func(attempt int, pending string) {
			switch attempt {
			case 1:
				guard = holdWindowsBootValidatorGuard(t, pending)
			case 2:
				guard.release()
			}
		})
		err := c.stopServiceChildren(t.Context())
		if guard != nil {
			guard.release()
		}
		if err != nil {
			t.Fatal("Stop did not survive a released validator guard on its pending manifest source")
		}
		if *attempts < 2 {
			t.Fatal("Stop did not retry its manifest publication around the released pending source guard")
		}
		windowsManifestStopped(t, c)
	})

	t.Run("held_past_budget", func(t *testing.T) {
		c, _ := unitCell(t)
		path := c.path("manifest.json")
		before := windowsBootPublishState(t, path)
		guard := holdWindowsBootValidatorGuard(t, path)
		attempts := observeWindowsManifestPublications(t, c, func(int, string) {})
		started := time.Now()
		err := c.stopServiceChildren(t.Context())
		elapsed := time.Since(started)
		guard.release()
		windowsManifestSharingRefusal(t, err, "Stop lost the sharing violation of a manifest guard held past its budget")
		// The lease bounds: a two-second budget with 25ms backoff doubling to
		// 200ms allows at most fourteen attempts.
		if *attempts < 2 || *attempts > 14 || elapsed < serviceLeasePublishBudget {
			t.Fatalf("held manifest guard: attempts=%d elapsed_ms=%d", *attempts, elapsed.Milliseconds())
		}
		windowsManifestUnchanged(t, c, before)
		// The refusal is clean: the interrupted stop stays pending for Repair.
		progress, err := c.readJournal()
		if err != nil || progress.pending == nil || progress.pending.Operation != "stop" {
			t.Fatal("refused Stop did not leave its operation pending for Repair")
		}
	})

	t.Run("validation_refusal_single_attempt", func(t *testing.T) {
		c, _ := unitCell(t)
		path := c.path("manifest.json")
		before := windowsBootPublishState(t, path)
		attempts := observeWindowsManifestPublications(t, c, func(attempt int, _ string) {
			// A readable but non-private manifest must fail validation during
			// publication and must not be retried.
			if attempt == 1 && !setTestDACL(t, path, "(A;;FR;;;WD)", false, false) {
				t.Error("cannot install manifest validation refusal fixture")
			}
		})
		err := c.stopServiceChildren(t.Context())
		if err == nil || runtimeconfig.WindowsPrivateFailureClass(err) != "validation" || *attempts != 1 {
			t.Fatalf("manifest validation refusal: failure=%t class=%s attempts=%d", err != nil, runtimeconfig.WindowsPrivateFailureClass(err), *attempts)
		}
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, before.data) {
			t.Fatal("manifest validation refusal changed the published manifest")
		}
	})
}

func windowsManifestSharingRefusal(t *testing.T, err error, message string) {
	t.Helper()
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || runtimeconfig.WindowsPrivateFailureClass(err) != "sharing_violation" {
		t.Fatal(message)
	}
}

// Bytes, pinned identity and a valid manifest: nothing was published.
func windowsManifestUnchanged(t *testing.T, c *Cell, before windowsBootPublishSnapshot) {
	t.Helper()
	after := windowsBootPublishState(t, c.path("manifest.json"))
	if !bytes.Equal(before.data, after.data) || !os.SameFile(before.info, after.info) {
		t.Fatal("refused manifest publication changed the published manifest")
	}
	if _, err := readManifest(c.cfg.Root); err != nil {
		t.Fatal("refused manifest publication left an unreadable manifest")
	}
}

func windowsManifestStopped(t *testing.T, c *Cell) {
	t.Helper()
	m, err := readManifest(c.cfg.Root)
	if err != nil || m.LastOperation != "stop" {
		t.Fatal("Stop did not publish its stopped manifest")
	}
	progress, err := c.readJournal()
	if err != nil || progress.pending != nil || progress.tornTail {
		t.Fatal("Stop did not settle its journal operation")
	}
}

// Every manifest publication attempt syncs a fresh pending source in the root
// exactly once through the existing syncFile seam; Stop's journal records sync
// in journal/ and are not counted. observe runs with the attempt number before
// that sync, which itself is unchanged.
func observeWindowsManifestPublications(t *testing.T, c *Cell, observe func(attempt int, pending string)) *int {
	t.Helper()
	root := filepath.Dir(c.path("manifest.json"))
	original := syncFile
	t.Cleanup(func() { syncFile = original })
	attempts := 0
	syncFile = func(f *os.File) error {
		if filepath.Dir(f.Name()) == root && strings.HasPrefix(filepath.Base(f.Name()), ".pending-") {
			attempts++
			observe(attempts, f.Name())
		}
		return original(f)
	}
	return &attempts
}
