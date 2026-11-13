//go:build windows

package localcell

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// A private-tree validation (for example the readiness client reading the
// control secret) holds each object it checks through runtimeconfig's path
// guard. Hold that exact guard on the published boot record or on the pending
// source while boot reconciliation publishes, and release it only at a fixed
// point: the second attempt's source sync. No sleep synchronizes this proof.
func TestWindowsServiceBootPublicationValidatorGuard(t *testing.T) {
	t.Run("single_attempt_published_target", func(t *testing.T) {
		_, d, path := windowsBootPublishRestartFixture(t)
		before := windowsBootPublishState(t, path)
		boot := windowsBootPublishID(t)
		guard := holdWindowsBootValidatorGuard(t, path)
		// The publication before FLAKE-35B was exactly one such attempt;
		// writeAtomic now repeats it only through its shared bounded retry.
		err := writeAtomicOnce(path, serviceBootRecord{d.InstallationID, boot}, nil)
		guard.release()
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || runtimeconfig.WindowsPrivateFailureClass(err) != "sharing_violation" {
			t.Fatal("validator guard on the boot record did not refuse a single publication with a sharing violation")
		}
		before.unchanged(t, path)
	})

	t.Run("single_attempt_pending_source", func(t *testing.T) {
		c, d := descriptorTestCell(t)
		path := c.path("service", "boot.json")
		boot := windowsBootPublishID(t)
		var guard *windowsBootValidatorGuard
		attempts := observeWindowsBootPublications(t, c, func(attempt int, pending string) {
			if attempt == 1 {
				guard = holdWindowsBootValidatorGuard(t, pending)
			}
		})
		err := writeAtomicOnce(path, serviceBootRecord{d.InstallationID, boot}, nil)
		if guard == nil || *attempts != 1 {
			t.Fatal("single boot publication did not reach its pending source once")
		}
		guard.release()
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || runtimeconfig.WindowsPrivateFailureClass(err) != "sharing_violation" {
			t.Fatal("validator guard on the pending source did not refuse a single publication with a sharing violation")
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("refused first boot publication exposed a boot record")
		}
	})

	t.Run("retry_published_target_released", func(t *testing.T) {
		c, d, path := windowsBootPublishRestartFixture(t)
		before := windowsBootPublishState(t, path)
		guard := holdWindowsBootValidatorGuard(t, path)
		attempts := observeWindowsBootPublications(t, c, func(attempt int, _ string) {
			if attempt == 2 {
				guard.release()
			}
		})
		err := c.reconcileServiceBoot()
		guard.release()
		if err != nil {
			t.Fatal("boot reconciliation did not survive a released validator guard on the boot record")
		}
		// The guard refuses the first attempt; the second runs after release. A
		// foreign handle on a CI runner may add attempts, never remove one.
		if *attempts < 2 {
			t.Fatal("boot reconciliation did not retry around the released boot record guard")
		}
		after := windowsBootPublishState(t, path)
		if !bytes.Equal(after.data, windowsBootPublishBytes(t, d, windowsBootPublishID(t))) || os.SameFile(before.info, after.info) {
			t.Fatal("boot reconciliation did not replace the guarded boot record")
		}
	})

	t.Run("retry_pending_source_released", func(t *testing.T) {
		c, d := descriptorTestCell(t)
		path := c.path("service", "boot.json")
		var guard *windowsBootValidatorGuard
		attempts := observeWindowsBootPublications(t, c, func(attempt int, pending string) {
			switch attempt {
			case 1:
				guard = holdWindowsBootValidatorGuard(t, pending)
			case 2:
				guard.release()
			}
		})
		err := c.reconcileServiceBoot()
		if guard != nil {
			guard.release()
		}
		if err != nil {
			t.Fatal("boot reconciliation did not survive a released validator guard on the pending source")
		}
		if *attempts < 2 {
			t.Fatal("boot reconciliation did not retry around the released pending source guard")
		}
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, windowsBootPublishBytes(t, d, windowsBootPublishID(t))) {
			t.Fatal("first boot reconciliation did not publish the expected boot record")
		}
	})

	t.Run("held_past_budget", func(t *testing.T) {
		c, _, path := windowsBootPublishRestartFixture(t)
		before := windowsBootPublishState(t, path)
		guard := holdWindowsBootValidatorGuard(t, path)
		attempts := observeWindowsBootPublications(t, c, func(int, string) {})
		started := time.Now()
		err := c.reconcileServiceBoot()
		elapsed := time.Since(started)
		guard.release()
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || runtimeconfig.WindowsPrivateFailureClass(err) != "sharing_violation" {
			t.Fatal("boot reconciliation lost the sharing violation of a guard held past its budget")
		}
		// The lease bounds: a two-second budget with 25ms backoff doubling to
		// 200ms allows at most fourteen attempts.
		if *attempts < 2 || *attempts > 14 || elapsed < serviceLeasePublishBudget {
			t.Fatalf("held boot record guard: attempts=%d elapsed_ms=%d", *attempts, elapsed.Milliseconds())
		}
		before.unchanged(t, path)
	})

	t.Run("validation_refusal_single_attempt", func(t *testing.T) {
		c, _, path := windowsBootPublishRestartFixture(t)
		before := windowsBootPublishState(t, path)
		attempts := observeWindowsBootPublications(t, c, func(attempt int, _ string) {
			// A readable but non-private boot record must fail validation
			// during publication and must not be retried.
			if attempt == 1 && !setTestDACL(t, path, "(A;;FR;;;WD)", false, false) {
				t.Error("cannot install boot validation refusal fixture")
			}
		})
		err := c.reconcileServiceBoot()
		if err == nil || runtimeconfig.WindowsPrivateFailureClass(err) != "validation" || *attempts != 1 {
			t.Fatalf("boot validation refusal: failure=%t class=%s attempts=%d", err != nil, runtimeconfig.WindowsPrivateFailureClass(err), *attempts)
		}
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, before.data) {
			t.Fatal("boot validation refusal changed the published boot record")
		}
	})
}

// The restart fixture is the native CI generation that replaces boot.json:
// the first reconciliation published it in this same boot.
func windowsBootPublishRestartFixture(t *testing.T) (*Cell, ServiceDescriptor, string) {
	t.Helper()
	c, d := descriptorTestCell(t)
	if err := c.reconcileServiceBoot(); err != nil {
		t.Fatal("cannot publish the first boot record fixture")
	}
	return c, d, c.path("service", "boot.json")
}

func windowsBootPublishID(t *testing.T) string {
	t.Helper()
	boot, err := serviceBootID()
	if err != nil {
		t.Fatal("native boot identity unavailable")
	}
	return boot
}

// writeAtomic's encoding: indented JSON plus one final newline.
func windowsBootPublishBytes(t *testing.T, d ServiceDescriptor, boot string) []byte {
	t.Helper()
	data, err := json.MarshalIndent(serviceBootRecord{d.InstallationID, boot}, "", "  ")
	if err != nil {
		t.Fatal("cannot encode expected boot record")
	}
	return append(data, '\n')
}

type windowsBootPublishSnapshot struct {
	data []byte
	info os.FileInfo
}

func windowsBootPublishState(t *testing.T, path string) windowsBootPublishSnapshot {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read boot record fixture")
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal("cannot inspect boot record fixture")
	}
	// Windows resolves FileInfo identity lazily through its path. Pin it now so
	// a later replacement cannot make the old snapshot observe the new object.
	if !os.SameFile(info, info) {
		t.Fatal("cannot pin boot record fixture identity")
	}
	return windowsBootPublishSnapshot{data: data, info: info}
}

func (s windowsBootPublishSnapshot) unchanged(t *testing.T, path string) {
	t.Helper()
	after := windowsBootPublishState(t, path)
	if !bytes.Equal(s.data, after.data) || !os.SameFile(s.info, after.info) {
		t.Fatal("refused boot publication changed the published boot record")
	}
	var record serviceBootRecord
	if readJSON(path, &record) != nil {
		t.Fatal("refused boot publication left an unreadable boot record")
	}
}

type windowsBootValidatorGuard struct {
	t      *testing.T
	handle windows.Handle
}

// Open path exactly as windowsPrivateGuard opens each component it validates
// (internal/runtimeconfig/windows_private.go): FILE_TRAVERSE takes part in
// sharing checks, and read/write sharing without delete sharing refuses every
// DELETE open, so every replacement, until the handle closes. A portable
// source contract pins these arguments to that guard.
func holdWindowsBootValidatorGuard(t *testing.T, path string) *windowsBootValidatorGuard {
	t.Helper()
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal("cannot encode boot validator guard fixture")
	}
	handle, err := windows.CreateFile(pointer, windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal("cannot hold boot validator guard fixture")
	}
	guard := &windowsBootValidatorGuard{t: t, handle: handle}
	t.Cleanup(guard.release)
	return guard
}

func (g *windowsBootValidatorGuard) release() {
	if g.handle == windows.InvalidHandle {
		return
	}
	err := windows.CloseHandle(g.handle)
	g.handle = windows.InvalidHandle
	if err != nil {
		g.t.Error("cannot release boot validator guard fixture")
	}
}

// Every boot publication attempt syncs a fresh pending source in service/
// exactly once through the existing syncFile seam. observe runs with the
// attempt number before that sync, which itself is unchanged.
func observeWindowsBootPublications(t *testing.T, c *Cell, observe func(attempt int, pending string)) *int {
	t.Helper()
	original := syncFile
	t.Cleanup(func() { syncFile = original })
	attempts := 0
	syncFile = func(f *os.File) error {
		if filepath.Dir(f.Name()) == c.path("service") && strings.HasPrefix(filepath.Base(f.Name()), ".pending-") {
			attempts++
			observe(attempts, f.Name())
		}
		return original(f)
	}
	return &attempts
}
