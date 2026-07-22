//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

const copilotOperationLockFile = ".witself-copilot-operation.lock"

func acquireCopilotOperationLock() (func(), error) {
	configRoot, err := currentCopilotConfigRoot()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(configRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create GitHub Copilot config root: %w", err)
	}

	path := filepath.Join(configRoot, copilotOperationLockFile)
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open GitHub Copilot integration lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect GitHub Copilot integration lock: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("GitHub Copilot integration lock is not a regular file")
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("secure GitHub Copilot integration lock: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errors.New("another GitHub Copilot install, uninstall, or routing refresh is already running")
		}
		return nil, fmt.Errorf("lock GitHub Copilot integration: %w", err)
	}

	closeFile = false
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = unix.Flock(fd, unix.LOCK_UN)
			_ = file.Close()
		})
	}, nil
}
