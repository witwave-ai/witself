package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// acquireIntegrationOperationLock opens one persistent, state-free lock file
// without following its final path component and takes a nonblocking exclusive
// lock. The provider-specific wrapper supplies the operator-facing busy error.
func acquireIntegrationOperationLock(path, label, busyMessage string) (func(), error) {
	if path == "" || strings.TrimSpace(path) != path || strings.ContainsAny(path, "\x00\r\n") {
		return nil, fmt.Errorf("%s integration lock path is invalid", label)
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%s integration lock path must be absolute", label)
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create %s integration lock directory: %w", label, err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect %s integration lock directory: %w", label, err)
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s integration lock directory is not a real directory", label)
	}

	file, err := openIntegrationOperationLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("open %s integration lock: %w", label, err)
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()

	if err := validateIntegrationOperationLockFile(path, file); err != nil {
		return nil, fmt.Errorf("inspect %s integration lock: %w", label, err)
	}
	if err := secureIntegrationOperationLockFile(file); err != nil {
		return nil, fmt.Errorf("secure %s integration lock: %w", label, err)
	}
	acquired, err := tryIntegrationOperationLock(file)
	if err != nil {
		return nil, fmt.Errorf("lock %s integration: %w", label, err)
	}
	if !acquired {
		return nil, errors.New(busyMessage)
	}
	if err := validateIntegrationOperationLockFile(path, file); err != nil {
		_ = unlockIntegrationOperationLock(file)
		return nil, fmt.Errorf("revalidate locked %s integration file: %w", label, err)
	}

	closeFile = false
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = unlockIntegrationOperationLock(file)
			_ = file.Close()
		})
	}, nil
}

func validateIntegrationOperationLockFile(path string, file *os.File) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() {
		return errors.New("lock path is not a regular file")
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !linked.Mode().IsRegular() || linked.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, linked) {
		return errors.New("lock path changed while it was being opened")
	}
	return validateIntegrationOperationLockIdentity(file)
}
