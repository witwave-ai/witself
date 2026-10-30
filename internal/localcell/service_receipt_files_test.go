//go:build !windows

package localcell_test

import (
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These helpers are shared with the Linux owner receipt, but their tests never
// start a lifecycle. Inventory paths and digests remain in memory only.
type ownerReceiptEntry struct {
	Directory bool
	Size      int64
	Digest    [sha256.Size]byte
}

func ownerReceiptInventory(root string) (map[string]ownerReceiptEntry, error) {
	invalid := errors.New("cannot inventory stopped receipt data")
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return nil, invalid
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		return nil, invalid
	}
	defer func() { _ = directory.Close() }()
	entries := make(map[string]ownerReceiptEntry)
	err = fs.WalkDir(directory.FS(), ".", func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return invalid
		}
		info, err := directory.Lstat(filepath.FromSlash(path))
		if err != nil {
			return invalid
		}
		if info.IsDir() {
			entries[path] = ownerReceiptEntry{Directory: true}
			return nil
		}
		if !info.Mode().IsRegular() {
			return invalid
		}
		file, err := directory.Open(filepath.FromSlash(path))
		if err != nil {
			return invalid
		}
		opened, statErr := file.Stat()
		if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			_ = file.Close()
			return invalid
		}
		hash := sha256.New()
		size, readErr := io.Copy(hash, file)
		after, afterErr := file.Stat()
		closeErr := file.Close()
		if readErr != nil || afterErr != nil || closeErr != nil || size != opened.Size() ||
			after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
			return invalid
		}
		entry := ownerReceiptEntry{Size: size}
		copy(entry.Digest[:], hash.Sum(nil))
		entries[path] = entry
		return nil
	})
	if err != nil {
		return nil, invalid
	}
	return entries, nil
}

func ownerReceiptInventoryEqual(before, after map[string]ownerReceiptEntry) bool {
	if len(before) == 0 || len(before) != len(after) {
		return false
	}
	for path, entry := range before {
		if other, exists := after[path]; !exists || entry != other {
			return false
		}
	}
	return true
}

func ownerReceiptWriteOpened(file *os.File, data []byte, owned func(os.FileInfo) bool) error {
	info, err := file.Stat()
	if err != nil || info.Mode() != 0o600 || owned == nil || !owned(info) {
		return errors.New("receipt record must be a private owned regular file")
	}
	if _, err := file.Write(data); err != nil {
		return errors.New("cannot write private receipt record")
	}
	if file.Sync() != nil {
		return errors.New("cannot persist private receipt record")
	}
	return nil
}

func ownerReceiptWritePrivate(path string, data []byte, owned func(os.FileInfo) bool) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("cannot create private receipt record")
	}
	writeErr := ownerReceiptWriteOpened(file, data, owned)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return errors.New("cannot close private receipt record")
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errors.New("cannot open receipt directory for persistence")
	}
	syncErr, closeErr := directory.Sync(), directory.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("cannot persist receipt directory")
	}
	return nil
}

func ownerReceiptRemoveArtifacts(root string, names []string) error {
	invalid := errors.New("cannot remove private receipt artifacts")
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return invalid
	}
	seen := make(map[string]bool)
	for _, name := range names {
		switch name {
		case ".receipt-build", "home", "receipt":
		default:
			return invalid
		}
		relative, err := filepath.Rel(root, filepath.Join(root, name))
		if err != nil || relative == "." || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) || seen[name] {
			return invalid
		}
		seen[name] = true
	}
	// Validate every requested child before deleting any child. The confined
	// handle also prevents a replaced descendant from escaping through a link.
	directory, err := os.OpenRoot(root)
	if err != nil {
		return invalid
	}
	defer func() { _ = directory.Close() }()
	for _, name := range names {
		if directory.RemoveAll(name) != nil {
			return invalid
		}
	}
	return nil
}

func ownerReceiptCleanupArtifacts(root string) error {
	return ownerReceiptRemoveArtifacts(root, []string{".receipt-build", "home", "receipt"})
}

// The only harness invocations the receipt needs are registration and removal.
// Reject manual lifecycle actions even if a future driver edit bypasses the
// phase-2 trace; the callback is never invoked for a refused action.
func ownerReceiptInvokeAllowed(action string, invoke func() bool) bool {
	return (action == "install" || action == "remove") && invoke()
}

func ownerReceiptSyntheticData(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if os.MkdirAll(filepath.Join(root, "base", "database"), 0o700) != nil ||
		os.WriteFile(filepath.Join(root, "PG_VERSION"), []byte("16\n"), 0o600) != nil ||
		os.WriteFile(filepath.Join(root, "base", "database", "relation"), []byte("synthetic row"), 0o600) != nil {
		t.Fatal("cannot prepare synthetic receipt data")
	}
	return root
}

func TestOwnerReceiptStoppedInventory(t *testing.T) {
	for _, scenario := range []string{"unchanged", "delete_base", "truncate", "same_size_edit", "empty_directory_added"} {
		ownerReceiptRequiredRun(t, scenario, func(t *testing.T) {
			root := ownerReceiptSyntheticData(t)
			before, err := ownerReceiptInventory(root)
			if err != nil {
				t.Fatal("cannot take initial synthetic inventory")
			}
			switch scenario {
			case "delete_base":
				err = os.RemoveAll(filepath.Join(root, "base"))
			case "truncate":
				err = os.Truncate(filepath.Join(root, "base", "database", "relation"), 1)
			case "same_size_edit":
				err = os.WriteFile(filepath.Join(root, "base", "database", "relation"), []byte("different row"), 0o600)
			case "empty_directory_added":
				err = os.Mkdir(filepath.Join(root, "extra"), 0o700)
			}
			if err != nil {
				t.Fatal("cannot modify synthetic receipt data")
			}
			after, err := ownerReceiptInventory(root)
			if err != nil || ownerReceiptInventoryEqual(before, after) != (scenario == "unchanged") {
				t.Fatal("stopped receipt inventory comparison accepted an invalid outcome")
			}
		})
	}
	if ownerReceiptInventoryEqual(nil, nil) {
		t.Fatal("absent inventories must not establish retained data")
	}
}

func TestOwnerReceiptInventoryRejectsSymlinks(t *testing.T) {
	for _, scenario := range []string{"root", "file", "directory"} {
		ownerReceiptRequiredRun(t, scenario, func(t *testing.T) {
			root := ownerReceiptSyntheticData(t)
			target := root
			if scenario == "file" {
				target = filepath.Join(root, "PG_VERSION")
			}
			link := filepath.Join(root, "link")
			if scenario == "root" {
				link = filepath.Join(t.TempDir(), "link")
			}
			if os.Symlink(target, link) != nil {
				t.Fatal("cannot create synthetic inventory symlink")
			}
			if scenario == "root" {
				root = link
			}
			if _, err := ownerReceiptInventory(root); err == nil {
				t.Fatal("receipt inventory accepted a symlink")
			}
		})
	}
}

func TestOwnerReceiptPrivateWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if ownerReceiptWritePrivate(path, []byte("synthetic"), func(os.FileInfo) bool { return true }) != nil {
		t.Fatal("cannot write synthetic private receipt")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode() != 0o600 || info.Size() != int64(len("synthetic")) {
		t.Fatal("private receipt write did not preserve its contract")
	}
	if ownerReceiptWritePrivate(path, []byte("replacement"), func(os.FileInfo) bool { return true }) == nil {
		t.Fatal("private receipt write replaced an existing record")
	}
}

func TestOwnerReceiptPrivateWriteRejectsActualPermissions(t *testing.T) {
	for _, scenario := range []string{"actual_0644", "unowned", "nonregular"} {
		ownerReceiptRequiredRun(t, scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "record")
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				t.Fatal("cannot create synthetic private receipt")
			}
			defer func() { _ = file.Close() }()
			if scenario == "actual_0644" && file.Chmod(0o644) != nil {
				t.Fatal("cannot set actual synthetic receipt permissions")
			}
			if scenario == "nonregular" {
				_ = file.Close()
				file, err = os.Open(t.TempDir())
				if err != nil {
					t.Fatal("cannot open synthetic receipt directory")
				}
			}
			if ownerReceiptWriteOpened(file, []byte("synthetic"), func(os.FileInfo) bool { return scenario != "unowned" }) == nil {
				t.Fatal("private receipt write accepted unsafe file properties")
			}
			info, err := os.Stat(path)
			if err != nil || info.Size() != 0 {
				t.Fatal("private receipt bytes were written before verification")
			}
		})
	}
}

func TestOwnerReceiptCleanupContainment(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	if os.Mkdir(root, 0o700) != nil || os.Mkdir(outside, 0o700) != nil ||
		os.WriteFile(filepath.Join(outside, "sentinel"), []byte("synthetic"), 0o600) != nil ||
		os.Mkdir(filepath.Join(root, "receipt"), 0o700) != nil {
		t.Fatal("cannot prepare synthetic cleanup boundaries")
	}
	for _, name := range []string{"../outside", ".", "..", "pg", "receipt/../receipt", outside} {
		if ownerReceiptRemoveArtifacts(root, []string{"receipt", name}) == nil {
			t.Fatal("receipt cleanup accepted an unapproved child")
		}
		if _, err := os.Stat(filepath.Join(root, "receipt")); err != nil {
			t.Fatal("receipt cleanup mutated before validating all children")
		}
	}
	if os.Symlink(outside, filepath.Join(root, "home")) != nil ||
		ownerReceiptCleanupArtifacts(root) != nil {
		t.Fatal("cannot clean approved synthetic receipt artifacts")
	}
	if _, err := os.Stat(filepath.Join(outside, "sentinel")); err != nil {
		t.Fatal("receipt cleanup removed the sibling sentinel")
	}
	if _, err := os.Lstat(filepath.Join(root, "home")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("receipt cleanup did not remove the symlink leaf")
	}
}
