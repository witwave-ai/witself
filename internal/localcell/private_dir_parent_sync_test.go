package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func replacePrivateDirParentSync(t *testing.T, hook func(string) error) {
	t.Helper()
	original := syncPrivateDirParent
	t.Cleanup(func() { syncPrivateDirParent = original })
	syncPrivateDirParent = hook
}

func requirePrivateDirectory(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !privateOwnedDir(path, info) {
		t.Fatal("directory is missing, not a real directory, or not private and owned")
	}
}

func TestPrivateDirParentSyncDefaultsToSyncDirectory(t *testing.T) {
	if reflect.ValueOf(syncPrivateDirParent).Pointer() != reflect.ValueOf(syncDirectory).Pointer() {
		t.Fatal("private directory parent sync no longer defaults to syncDirectory")
	}
}

func TestPrivateDirParentSyncCreationOrder(t *testing.T) {
	parent := testPrivateRoot(t)
	path := filepath.Join(parent, "created")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("directory already exists before creation")
	}
	events := []string{"missing"}
	var paths []string
	replacePrivateDirParentSync(t, func(parentPath string) error {
		requirePrivateDirectory(t, path)
		events = append(events, "private_directory_exists", "parent_sync")
		paths = append(paths, parentPath)
		return syncDirectory(parentPath)
	})
	if err := privateDir(path, true); err != nil {
		t.Fatal(err)
	}
	events = append(events, "returned")
	if !slices.Equal(events, []string{"missing", "private_directory_exists", "parent_sync", "returned"}) {
		t.Fatalf("creation sync order = %v", events)
	}
	if !slices.Equal(paths, []string{parent}) {
		t.Fatal("creation did not sync exactly its parent once")
	}
}

func TestPrivateDirParentSyncSkipsExisting(t *testing.T) {
	path := testPrivateRoot(t)
	calls := 0
	replacePrivateDirParentSync(t, func(string) error {
		calls++
		return errors.New("unexpected parent sync")
	})
	for _, create := range []bool{true, false} {
		if err := privateDir(path, create); err != nil {
			t.Fatal("existing private directory was refused")
		}
	}
	if calls != 0 {
		t.Fatal("an existing directory synced its parent")
	}
}

func TestPrivateDirParentSyncMissingWithoutCreate(t *testing.T) {
	path := filepath.Join(testPrivateRoot(t), "missing")
	calls := 0
	replacePrivateDirParentSync(t, func(string) error {
		calls++
		return nil
	})
	err := privateDir(path, false)
	if err == nil || err.Error() != "local cell directory is missing or not a real directory" {
		t.Fatal("missing directory did not return the existing fixed error")
	}
	if calls != 0 {
		t.Fatal("missing directory synced its parent without creation")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("create=false created a directory")
	}
}

func TestPrivateDirParentSyncCreationFailure(t *testing.T) {
	path := filepath.Join(testPrivateRoot(t), "missing", "child")
	calls := 0
	replacePrivateDirParentSync(t, func(string) error {
		calls++
		return nil
	})
	err := privateDir(path, true)
	if err == nil || err.Error() != "cannot create private local cell directory" {
		t.Fatal("failed creation did not return the existing fixed error")
	}
	if calls != 0 {
		t.Fatal("failed creation synced a parent")
	}
}

func TestPrivateDirParentSyncFailureLeavesPrivateDirectory(t *testing.T) {
	parent := testPrivateRoot(t)
	path := filepath.Join(parent, "created")
	injected := errors.New("synthetic private parent sync details")
	var paths []string
	replacePrivateDirParentSync(t, func(parentPath string) error {
		requirePrivateDirectory(t, path)
		paths = append(paths, parentPath)
		return injected
	})
	err := privateDir(path, true)
	if err == nil || err.Error() != "cannot sync local cell parent directory" || errors.Is(err, injected) {
		t.Fatal("parent sync failure did not return the fixed value-free error")
	}
	requirePrivateDirectory(t, path)
	if !slices.Equal(paths, []string{parent}) {
		t.Fatal("failed sync did not target exactly the parent once")
	}
	// A failed sync retains the private directory. Finding it again validates it
	// without retrying the sync, even while the injected failure remains active.
	if err := privateDir(path, true); err != nil {
		t.Fatal("retained private directory was refused")
	}
	if len(paths) != 1 {
		t.Fatal("a later call retried the parent sync for an existing directory")
	}
}

func TestLayoutParentSyncOnlyForCreatedDirectories(t *testing.T) {
	root := testPrivateRoot(t)
	c := &Cell{cfg: Config{Root: root}}
	names := []string{"spool", "run", "journal", "logs", "tmp", "home"}
	var paths []string
	replacePrivateDirParentSync(t, func(parentPath string) error {
		if len(paths) >= len(names) {
			t.Fatal("layout synced more than once per created directory")
		}
		requirePrivateDirectory(t, c.path(names[len(paths)]))
		paths = append(paths, parentPath)
		return syncDirectory(parentPath)
	})
	if err := c.layout(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{root, root, root, root, root, root}) {
		t.Fatal("fresh layout did not sync the root exactly six times")
	}
	if err := c.layout(); err != nil {
		t.Fatal(err)
	}
	if len(paths) != len(names) {
		t.Fatal("existing layout synced its root")
	}
}
