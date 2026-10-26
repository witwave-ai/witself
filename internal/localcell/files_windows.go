//go:build windows

package localcell

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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

// A pre-existing untrusted object anywhere in the layout must fail before the
// operation creates a lock, appends a journal entry, or removes retained state.
// WalkDir does not traverse symlinks; validation also rejects every reparse tag.
func validatePrivateTree(root string) error {
	if err := validateRootPath(root); err != nil {
		return err
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return errors.New("cannot inspect private local cell layout")
		}
		pg := filepath.Join(root, "pg")
		if path == pg || strings.HasPrefix(path, pg+string(filepath.Separator)) {
			return runtimeconfig.WindowsPrivatePostgresValidate(pg, path, entry.IsDir())
		}
		return validatePrivate(path, entry.IsDir())
	})
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
