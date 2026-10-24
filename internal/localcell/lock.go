package localcell

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLocked means another operation currently owns this cell's root.
var ErrLocked = errors.New("localcell: another operation owns this cell")

type operationLock struct {
	files []*os.File
}

// acquireLock takes the stable root lock before the disposable run lock. The
// root lock is deliberately retained even by purge: unlinking a locked file
// would let another owner lock a replacement inode while the first still runs.
// A read-only probe never creates directories or files.
func acquireLock(root string, create bool) (_ *operationLock, err error) {
	if err := validatePrivateTree(root); err != nil {
		return nil, err
	}
	if err := lockDirectory(root); err != nil {
		return nil, err
	}
	lock := &operationLock{}
	defer func() {
		if err != nil {
			_ = lock.Close()
		}
	}()
	stable, err := openLockFile(filepath.Join(root, ".owner.lock"), create)
	if errors.Is(err, os.ErrNotExist) && !create {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("localcell: open owner lock: %w", err)
	}
	lock.files = append(lock.files, stable)
	if err := lockFile(stable); err != nil {
		return nil, err
	}
	run := filepath.Join(root, "run")
	if create {
		if err := makePrivateDir(run); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("localcell: create run directory: %w", err)
		}
	}
	if err := lockDirectory(run); err != nil {
		if errors.Is(err, os.ErrNotExist) && !create {
			return lock, nil
		}
		return nil, err
	}
	operation, err := openLockFile(filepath.Join(run, "operation.lock"), create)
	if errors.Is(err, os.ErrNotExist) && !create {
		return lock, nil
	}
	if err != nil {
		return nil, fmt.Errorf("localcell: open operation lock: %w", err)
	}
	lock.files = append(lock.files, operation)
	if err := lockFile(operation); err != nil {
		return nil, err
	}
	return lock, nil
}

func lockDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("localcell: lock directory must be a real directory")
	}
	return nil
}

func validateLockFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("localcell: lock must be a regular file")
	}
	return nil
}

func (lock *operationLock) Close() error {
	if lock == nil {
		return nil
	}
	var result error
	for i := len(lock.files) - 1; i >= 0; i-- {
		result = errors.Join(result, unlockFile(lock.files[i]), lock.files[i].Close())
	}
	lock.files = nil
	return result
}
