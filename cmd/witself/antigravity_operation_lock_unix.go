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

func acquireAntigravityOperationLock() (func(), error) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve Antigravity home: %w", err)
	}
	userHome, err = cleanAntigravityAbsolutePath("HOME", userHome)
	if err != nil {
		return nil, err
	}
	configRoot := filepath.Join(userHome, ".gemini", "config")
	if err := os.MkdirAll(configRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create Antigravity config root: %w", err)
	}
	path := filepath.Join(configRoot, ".witself-antigravity-operation.lock")
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("Antigravity integration lock is not a regular file")
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errors.New("another Antigravity install or uninstall is already running")
		}
		return nil, err
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
