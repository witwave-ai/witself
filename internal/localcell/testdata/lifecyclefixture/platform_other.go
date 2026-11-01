//go:build !windows

package main

import (
	"encoding/json" // FLAKE-3 fix 5 (W1)
	"fmt"
	"os"
	"os/signal"
	"path/filepath" // FLAKE-3 fix 5 (W1)
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
func fixturePostmasterRecord(data, _ string, epoch int64) []byte {
	return []byte(fmt.Sprintf("%d\n%s\n%d\n", os.Getpid(), data, epoch))
}

func stopFixturePostgres(data []byte, deadline time.Time) error {
	pid, err := strconv.Atoi(strings.Split(string(data), "\n")[0])
	if err != nil {
		return err
	}
	var record struct{ Root string }                                      // FLAKE-3 fix 5 (W1)
	recordData, err := os.ReadFile(filepath.Join("run", "postgres.json")) // FLAKE-3 fix 5 (W1)
	if err != nil {                                                       // FLAKE-3 fix 5 (W1)
		return err // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
	root, err := os.Getwd() // FLAKE-3 fix 5 (W1)
	if err != nil {         // FLAKE-3 fix 5 (W1)
		return err // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
	if json.Unmarshal(recordData, &record) != nil || record.Root != root { // FLAKE-3 fix 5 (W1)
		return fmt.Errorf("fixture process identity mismatch") // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer func() { _ = process.Release() }()
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return nil
		} else if err != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("fixture PostgreSQL shutdown timeout")
}
