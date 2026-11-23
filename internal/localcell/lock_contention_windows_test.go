//go:build windows

package localcell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsOwnershipFailureBusyClassification(t *testing.T) {
	t.Run("denies_read_sharing", windowsOwnershipDeniesReadSharing)
	t.Run("fully_shared_delete", func(t *testing.T) {
		c, _ := unitCell(t)
		holdTreeDelete(t, c.path("manifest.json"))
		// The holder remains open through pre-lock and post-lock validation.
		owner, err := c.own(t.Context(), false)
		if err != nil || owner == nil {
			t.Fatal("fully sharing DELETE holder blocked Windows ownership")
		}
		if err := owner.Close(); err != nil {
			t.Fatal("cannot release Windows ownership fixture lock")
		}
	})
}

func windowsOwnershipDeniesReadSharing(t *testing.T) {
	c, _ := unitCell(t)
	root := c.cfg.Root
	if err := privateDir(c.path("pg"), true); err != nil {
		t.Fatal("cannot complete Windows ownership fixture layout")
	}
	owner, err := acquireLock(context.Background(), root, true)
	if err != nil {
		t.Fatal("cannot acquire Windows ownership fixture lock")
	}
	t.Cleanup(func() {
		if owner != nil {
			_ = owner.Close()
		}
	})
	// Match the full-sharing holder's access, but deny the read sharing
	// required by the tree validator's FILE_EXECUTE access on a regular leaf.
	pointer, err := windows.UTF16PtrFromString(c.path("manifest.json"))
	if err != nil {
		t.Fatal("cannot encode Windows ownership blocker path")
	}
	blocker, err := windows.CreateFile(pointer, windows.DELETE|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal("cannot open Windows ownership DELETE blocker")
	}
	t.Cleanup(func() {
		if blocker != windows.InvalidHandle {
			_ = windows.CloseHandle(blocker)
		}
	})
	before := windowsTreeSnapshot(t, root)
	if err := c.validateRoot(false); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatal("Windows ownership fixture did not refuse tree validation for sharing")
	}
	contender, err := c.own(context.Background(), false)
	if contender != nil {
		_ = contender.Close()
		t.Fatal("Windows own returned a lock while owner held it")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatal("Windows own did not classify held owner refusal as busy")
	}
	if !reflect.DeepEqual(before, windowsTreeSnapshot(t, root)) {
		t.Fatal("Windows own busy refusal changed the private tree")
	}
	contender, err = c.ownExisting(context.Background())
	if contender != nil {
		_ = contender.Close()
		t.Fatal("Windows ownExisting returned a lock while owner held it")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatal("Windows ownExisting did not classify held owner refusal as busy")
	}
	if !reflect.DeepEqual(before, windowsTreeSnapshot(t, root)) {
		t.Fatal("Windows ownExisting busy refusal changed the private tree")
	}
	if err := owner.Close(); err != nil {
		t.Fatal("cannot release Windows ownership fixture lock")
	}
	owner = nil
	before = windowsTreeSnapshot(t, root)
	contender, err = c.own(context.Background(), false)
	if contender != nil {
		_ = contender.Close()
		t.Fatal("Windows own returned a lock while validation remained blocked")
	}
	if err == nil {
		t.Fatal("Windows ownerless sharing refusal became success")
	}
	if errors.Is(err, ErrLocked) {
		t.Fatal("Windows ownerless sharing refusal became busy")
	}
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatal("Windows ownerless refusal lost its original sharing cause")
	}
	if !reflect.DeepEqual(before, windowsTreeSnapshot(t, root)) {
		t.Fatal("Windows ownerless sharing refusal changed the private tree")
	}
	if err := windows.CloseHandle(blocker); err != nil {
		t.Fatal("cannot release Windows ownership DELETE blocker")
	}
	blocker = windows.InvalidHandle
	contender, err = c.own(context.Background(), false)
	if err != nil || contender == nil {
		if contender != nil {
			_ = contender.Close()
		}
		t.Fatal("Windows ownership did not recover after blocker release")
	}
	if err := contender.Close(); err != nil {
		t.Fatal("cannot close recovered Windows ownership lock")
	}

	empty := testPrivateRoot(t)
	sentinel := errors.New("Windows missing owner refusal")
	if classifyOwnershipFailure(empty, sentinel) != sentinel {
		t.Fatal("Windows missing owner probe replaced its original refusal")
	}
	if _, err := os.Lstat(filepath.Join(empty, ".owner.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Windows missing owner probe created a stable lock")
	}
	if _, err := os.Lstat(filepath.Join(empty, "run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Windows missing owner probe created a run directory")
	}
}
