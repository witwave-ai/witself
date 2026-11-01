//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func fixtureExecutable(name string) string { return name }
func fixtureMkdir(path string) error       { return os.Mkdir(path, 0o700) }
func fixtureOpenFile(path string, flags int) (*os.File, error) {
	return os.OpenFile(path, flags, 0o600)
}
func fixtureWriteFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }
func notifyFixtureStop(stopped chan<- os.Signal) {
	signal.Notify(stopped, syscall.SIGTERM, os.Interrupt)
}
func runFixtureStopHelper() bool { return false }
func fixturePostmasterRecord(data, _ string) []byte {
	return []byte(fmt.Sprintf("%d\n%s\n%d\n", os.Getpid(), data, time.Now().Unix()))
}

func stopFixturePostgres(data []byte) error {
	pid, err := strconv.Atoi(strings.Split(string(data), "\n")[0])
	if err != nil {
		return err
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer func() { _ = process.Release() }()
	return process.Signal(syscall.SIGTERM)
}
