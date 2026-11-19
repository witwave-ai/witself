//go:build windows

package localcell

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// FLAKE-63 on a real handle. A private creation keeps its rollback handle (DELETE,
// every sharing mode) until it has verified the object, as the fixture's server and
// worker create their launch counter below run/ while the boot binding walks the tree;
// the walk's guard denies delete sharing, which [MS-FSA] 2.1.5.1.2.2 then refuses.
func TestWindowsBootBindingTreeRetry(t *testing.T) {
	// Without a descriptor the binding returns before it reads the boot identity.
	readBoot := func() (string, error) { return "", errors.New("synthetic boot identity refusal") }
	t.Run("pair", func(t *testing.T) {
		c, counter := windowsTreeRetryCell(t)
		release := holdTreeDelete(t, counter)
		err := validatePrivateTree(c.cfg.Root)
		release()
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatal("a held delete handle did not refuse one tree walk for sharing")
		}
		if runtimeconfig.WindowsPrivateFailureClass(err) != "sharing_violation" {
			t.Fatal("a held delete handle refused one tree walk with another class")
		}
		if err.Error() != "private Windows sharing-violation" {
			t.Fatal("a held delete handle refused one tree walk with another text")
		}
	})
	t.Run("released", func(t *testing.T) {
		c, counter := windowsTreeRetryCell(t)
		release, hold, released := holdTreeDelete(t, counter), serviceLeasePublishBudget/8, make(chan struct{})
		go func() {
			defer close(released)
			time.Sleep(hold)
			release()
		}()
		started := time.Now()
		err := c.bindServiceProcessBootWithReader("server", readBoot)
		elapsed := time.Since(started)
		<-released
		if err != nil {
			t.Fatal("the boot binding did not survive a delete handle released within its budget")
		}
		if elapsed < hold {
			t.Fatal("the boot binding passed a tree walk while the delete handle was held")
		}
	})
	t.Run("held", func(t *testing.T) {
		c, counter := windowsTreeRetryCell(t)
		release, done, fused := holdTreeDelete(t, counter), make(chan struct{}), make(chan struct{})
		go func() { // fuse: a retry that ignored its budget ends in success instead of never
			defer close(fused)
			select {
			case <-done:
			case <-time.After(3 * serviceLeasePublishBudget):
				release()
			}
		}()
		started := time.Now()
		err := c.bindServiceProcessBootWithReader("server", readBoot)
		elapsed := time.Since(started)
		close(done)
		<-fused
		release()
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatal("a delete handle held past the budget did not end the boot binding in a sharing violation")
		}
		if runtimeconfig.WindowsPrivateFailureClass(err) != "sharing_violation" {
			t.Fatal("a delete handle held past the budget changed the boot binding refusal class")
		}
		if err.Error() != "private Windows sharing-violation" {
			t.Fatal("a delete handle held past the budget changed the boot binding refusal text")
		}
		var located *serviceFailure
		if !errors.As(err, &located) || located.site != "boot_binding" {
			t.Fatal("a delete handle held past the budget lost the boot binding site")
		}
		if elapsed < serviceLeasePublishBudget {
			t.Fatal("the boot binding refused a held delete handle before its budget")
		}
	})
	t.Run("validation", func(t *testing.T) {
		c, counter := windowsTreeRetryCell(t)
		if !setTestDACL(t, counter, "(A;;FR;;;WD)", false, false) {
			t.Fatal("cannot install the tree validation refusal fixture")
		}
		started := time.Now()
		err := c.bindServiceProcessBootWithReader("server", readBoot)
		if runtimeconfig.WindowsPrivateFailureClass(err) != "validation" {
			t.Fatal("a non-private record did not refuse the boot binding by validation")
		}
		if time.Since(started) >= serviceLeasePublishBudget {
			t.Fatal("the boot binding retried a validation refusal")
		}
	})
}

// A descriptor-free local cell, as at the fixture's first Start, with a launch counter.
func windowsTreeRetryCell(t *testing.T) (*Cell, string) {
	t.Helper()
	c, _ := unitCell(t)
	t.Setenv("WITSELF_RUNTIME", "local")
	counter := c.path("run", "fixture-launches-server")
	if testWriteFile(counter, []byte("1"), 0o600) != nil {
		t.Fatal("cannot create the launch counter fixture")
	}
	return c, counter
}

// holdTreeDelete holds path as the rollback handle shares it: DELETE, all sharing modes.
func holdTreeDelete(t *testing.T, path string) func() {
	t.Helper()
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal("cannot encode the delete holder fixture")
	}
	handle, err := windows.CreateFile(pointer, windows.DELETE|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal("cannot hold the delete holder fixture")
	}
	release := sync.OnceFunc(func() {
		if windows.CloseHandle(handle) != nil {
			t.Error("cannot release the delete holder fixture")
		}
	})
	t.Cleanup(release)
	return release
}
