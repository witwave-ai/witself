package localcell

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// A WalkDir entry can disappear between enumeration and opened-object
// validation when PostgreSQL removes a transient file or the host publishes an
// atomic replacement. An incomplete pass never establishes privacy: retry the
// entire tree, with the same validators and a fixed limit. Missing directories,
// ancestors, and privacy refusals are not snapshot churn.
func validatePrivateTreeSnapshot(root string, walk func(string, fs.WalkDirFunc) error, validate func(string, bool) error, stat func(string) (os.FileInfo, error)) error {
	var result error
	for range 3 {
		retry := false
		result = walk(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry == nil {
				return errors.New("cannot inspect private local cell layout")
			}
			err := validate(path, entry.IsDir())
			if err == nil || !errors.Is(err, os.ErrNotExist) || path == root || !entry.Type().IsRegular() {
				return err
			}
			parent := filepath.Dir(path)
			info, parentErr := stat(parent)
			if parentErr != nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return err
			}
			if parentErr := validate(parent, true); parentErr != nil {
				return parentErr
			}
			retry = true
			return err
		})
		if !retry {
			return result
		}
	}
	return result
}
