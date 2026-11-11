package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// classifyOwnershipFailure reports a pre-lock ownership refusal as ErrLocked
// when another operation demonstrably holds the stable owner lock. Windows tree
// validation can collide with a live owner's handles before the first lock
// attempt; a busy cell must not be charged as a failed one.
func classifyOwnershipFailure(root string, original error) error {
	if runtime.GOOS != "windows" {
		return original
	}
	return probeOwnershipFailure(root, original, lockDirectory, openLockFile, lockFile, unlockFile)
}

func probeOwnershipFailure(root string, original error, checkDir func(string) error,
	open func(string, bool) (*os.File, error), tryLock, unlock func(*os.File) error) error {
	if original == nil || errors.Is(original, ErrLocked) {
		return original
	}
	if err := checkDir(root); err != nil {
		return original
	}
	file, err := open(filepath.Join(root, ".owner.lock"), false)
	if err != nil {
		return original
	}
	defer func() { _ = file.Close() }()
	if err := tryLock(file); errors.Is(err, ErrLocked) {
		return ErrLocked
	} else if err == nil {
		_ = unlock(file)
	}
	return original
}
