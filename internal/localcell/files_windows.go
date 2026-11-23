//go:build windows

package localcell

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

func makePrivateDir(path string) error { return runtimeconfig.WindowsPrivateMkdir(path) }

func privatePassword(path string, _ os.FileInfo) bool { return validatePrivate(path, false) == nil }

func validateRootPath(path string) error { return runtimeconfig.WindowsPrivateValidatePath(path) }

func validatePrivate(path string, directory bool) error {
	return runtimeconfig.WindowsPrivateValidate(path, directory)
}

func openPrivateFile(path string, flags int, _ os.FileMode) (*os.File, error) {
	if err := validateWriteTarget(path); err != nil {
		return nil, err
	}
	return runtimeconfig.WindowsPrivateOpenFile(path, flags)
}

// openPrivateReader is the guarded open: no FIFO can stand at a file-system
// name here, and it opens a link at the last element as itself and refuses it.
func openPrivateReader(_ context.Context, path string) (*os.File, error) {
	return openPrivateFile(path, os.O_RDONLY, 0)
}

// Snapshots serve read-only admission. In particular the endpoint owner still
// uses the ordinary guarded read before it updates or removes its record.
func openAdmissionRecord(_ context.Context, path string, open error) (*os.File, error) {
	return openWindowsServiceSnapshot(path, open, true)
}

func openWindowsServiceSnapshot(path string, open error, requireDirForMissing bool) (*os.File, error) {
	f, err := runtimeconfig.WindowsPrivateOpenSnapshot(path)
	if err == nil {
		return f, nil
	}
	// Only classify the refusal. A lookup here can never return a file or
	// authorize reading a byte. Preserve the public distinction between an
	// absent, non-private and otherwise unreadable service record.
	info, statErr := os.Lstat(path)
	if errors.Is(statErr, os.ErrNotExist) && (!requireDirForMissing || absentBelowDirectory(path)) {
		return nil, os.ErrNotExist
	}
	if statErr != nil || !info.Mode().IsRegular() || runtimeconfig.WindowsPrivateValidateSnapshot(path) != nil {
		return nil, errors.New("service_record_not_private")
	}
	return nil, open
}

func readServiceRecordJSON(_ context.Context, path string, dst any) error {
	// servicePrivateRecord historically classified ERROR_PATH_NOT_FOUND as
	// missing even below a non-directory, before readJSON inspected the name.
	f, err := openWindowsServiceSnapshot(path, errors.New("cannot open local cell record"), false)
	if err != nil {
		return err
	}
	return decodeLocalRecord(f, dst)
}

func readManifestJSON(ctx context.Context, path string, dst any) error {
	f, err := openRegularBy(ctx, func(_ context.Context, path string) (*os.File, error) {
		return runtimeconfig.WindowsPrivateOpenSnapshot(path)
	}, nil, path, errors.New("cannot inspect local cell record"),
		errors.New("local cell record must be a regular file"), errors.New("cannot open local cell record"))
	if err != nil {
		return err
	}
	return decodeLocalRecord(f, dst)
}

// openPrivateWriter is the guarded open too: it refuses flags outside its list,
// so the Unix non-blocking and no-follow flags never reach it.
func openPrivateWriter(_ context.Context, path string, flags int, mode os.FileMode) (*os.File, error) {
	return openPrivateFile(path, flags, mode)
}

// blockingWriter has nothing to clear: the guarded open is never non-blocking.
func blockingWriter(*os.File) error { return nil }

// openPlainReader keeps os.Open for a service manager file outside the root,
// which the guarded open would refuse; no FIFO can stand at its name here.
func openPlainReader(_ context.Context, path string) (*os.File, error) { return os.Open(path) }

func readPrivateFile(path string) ([]byte, error) {
	f, err := openPrivateFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

func createPrivateTemp(dir string) (*os.File, error) {
	return runtimeconfig.WindowsPrivateCreateTemp(dir)
}

func privateTempName(name string) bool { return windowsPrivateTempName(name) }

// A pre-existing untrusted object anywhere in the layout must fail before the
// operation creates a lock, appends a journal entry, or removes retained state.
// The one exception is content below <root>/recovery: that directory is
// validated as a single object and never descended, and what it holds is
// validated only by the operations that use it. Direct-child case aliases of
// recovery are refused before opening the alias or descending into it.
// WalkDir does not traverse symlinks; validation also rejects every reparse tag.
func validatePrivateTree(root string) error {
	if err := validateRootPath(root); err != nil {
		return err
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return validatePrivateTreeSnapshot(root, filepath.WalkDir, func(path string, directory bool) error {
		// Only the real validators are wired here: privateTreeEntry chooses the
		// rule and validates <root>/recovery as one object without descending.
		return privateTreeEntry(root, path, directory, func() error {
			if directory {
				return validatePrivate(path, true)
			}
			return runtimeconfig.WindowsPrivateValidateSnapshot(path)
		}, func(pg string) error {
			return runtimeconfig.WindowsPrivatePostgresValidate(pg, path, directory)
		})
	}, os.Lstat)
}

func validateWriteTarget(path string) error {
	if err := validateRootPath(path); err != nil {
		return err
	}
	if err := validatePrivate(filepath.Dir(path), true); err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return validatePrivate(path, false)
}

func publishPrivateFile(oldPath, newPath string) error {
	return runtimeconfig.WindowsPrivateRename(oldPath, newPath)
}

func removePrivateFile(path string) error { return runtimeconfig.WindowsPrivateRemove(path) }
