//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type captureFlushProcessIdentity struct {
	PID  int `json:"pid"`
	PGID int `json:"pgid"`
	SID  int `json:"sid"`
}

func captureFlushHelperExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CAPTURE_FLUSH_TEST_BINARY", executable)
	directory := t.TempDir()
	t.Setenv("CAPTURE_FLUSH_TEST_DIRECTORY", directory)
	wrapper := filepath.Join(directory, "witself-flush-helper")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec \"$CAPTURE_FLUSH_TEST_BINARY\" -test.run='^TestCaptureFlushHelperProcess$' -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(witselfExecutableTestEnv, wrapper)
	return directory
}

func readCaptureFlushTestFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			return raw
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for helper file %s", filepath.Base(path))
	return nil
}

func TestBackgroundCaptureFlushSurvivesParentExitAndHangup(t *testing.T) {
	directory := captureFlushHelperExecutable(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	parent := exec.CommandContext(ctx, os.Getenv("CAPTURE_FLUSH_TEST_BINARY"), "-test.run=^TestCaptureFlushHelperProcess$")
	parent.Env = append(os.Environ(), "CAPTURE_FLUSH_HELPER_MODE=parent")
	// Model a headless runtime with its own session, which cleans up the whole
	// process group as its final hook finishes.
	parent.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err := parent.Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("short-lived parent exit = %v, want SIGHUP", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGHUP {
		t.Fatalf("short-lived parent status = %v, want SIGHUP", exitErr.Sys())
	}
	var child captureFlushProcessIdentity
	if err := json.Unmarshal(readCaptureFlushTestFile(t, filepath.Join(directory, "child-ready")), &child); err != nil {
		t.Fatal(err)
	}
	if child.PID == parent.Process.Pid || child.PGID != child.PID || child.SID != child.PID {
		t.Fatalf("flusher identity = %#v, parent pid = %d; want independent session/group", child, parent.Process.Pid)
	}
	// Only release the child after Wait proves the parent exited. The child's
	// final write therefore demonstrates work completed after parent death.
	if err := os.WriteFile(filepath.Join(directory, "parent-exited"), []byte("yes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := string(readCaptureFlushTestFile(t, filepath.Join(directory, "child-survived"))); got != "yes" {
		t.Fatalf("child completion = %q", got)
	}
}

func TestHookForegroundFlushHonorsBudget(t *testing.T) {
	directory := captureFlushHelperExecutable(t)
	t.Setenv("CAPTURE_FLUSH_HELPER_MODE", "stall")
	// An inherited detached marker must not bypass the foreground path.
	t.Setenv(captureDetachedFlushEnv, "1")
	started := time.Now()
	err := runHookForegroundFlush(transcriptcapture.RuntimeClaudeCode)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("stalled foreground flush unexpectedly succeeded")
	}
	if elapsed < hookForegroundFlushMaxDuration-500*time.Millisecond || elapsed > hookForegroundFlushMaxDuration+time.Second {
		t.Fatalf("stalled foreground flush took %s; budget = %s", elapsed, hookForegroundFlushMaxDuration)
	}
	pid, err := strconv.Atoi(string(readCaptureFlushTestFile(t, filepath.Join(directory, "stall-ready"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("bounded foreground child still exists after Wait: %v", err)
	}
}

func TestTerminalHookFlushesForegroundBeforeDetached(t *testing.T) {
	for _, hook := range []string{"Stop", "SessionEnd"} {
		for _, failForeground := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fail_foreground=%t", hook, failForeground), func(t *testing.T) {
				directory := captureFlushHelperExecutable(t)
				t.Setenv("CAPTURE_FLUSH_HELPER_MODE", "record")
				t.Setenv("CAPTURE_FLUSH_FAIL_FOREGROUND", strconv.FormatBool(failForeground))
				t.Setenv("WITSELF_HOME", filepath.Join(directory, "state"))
				t.Setenv("WITSELF_CURATOR_SESSION", "")
				t.Setenv("WITSELF_CAPTURE_NO_FLUSH", "")
				location, err := transcriptcapture.EnsureLocation("test")
				if err != nil {
					t.Fatal(err)
				}
				if err := transcriptcapture.SaveConfig(transcriptcapture.Config{
					Runtime: transcriptcapture.RuntimeClaudeCode, CaptureMode: transcriptcapture.ModeRaw,
					HookMode: transcriptcapture.HookModeUser, Account: "default", Realm: "default",
					Agent: "scott", AgentID: "agent_1", AgentName: "scott", Location: location,
				}); err != nil {
					t.Fatal(err)
				}
				input, err := os.CreateTemp(directory, "hook-input")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = input.Close() }()
				if _, err := fmt.Fprintf(input, `{"session_id":"terminal-session","hook_event_name":%q}`, hook); err != nil {
					t.Fatal(err)
				}
				if _, err := input.Seek(0, 0); err != nil {
					t.Fatal(err)
				}
				priorStdin := os.Stdin
				os.Stdin = input
				t.Cleanup(func() { os.Stdin = priorStdin })
				stdout, stderr, code := captureFactDeleteCLI(t, func() int {
					return transcriptHook([]string{"--runtime", transcriptcapture.RuntimeClaudeCode})
				})
				if code != 0 || stdout != "" || stderr != "" {
					t.Fatalf("terminal hook = %d stdout=%q stderr=%q", code, stdout, stderr)
				}
				if got := string(readCaptureFlushTestFile(t, filepath.Join(directory, "detached-observed"))); got != "foreground\ndetached\n" {
					t.Fatalf("flush order = %q", got)
				}
			})
		}
	}
}

// TestCaptureFlushHelperProcess runs only in short-lived subprocesses. The
// executable wrapper preserves the production flusher's argv and environment.
func TestCaptureFlushHelperProcess(t *testing.T) {
	mode := os.Getenv("CAPTURE_FLUSH_HELPER_MODE")
	if mode == "" {
		return
	}
	directory := os.Getenv("CAPTURE_FLUSH_TEST_DIRECTORY")
	if mode == "parent" {
		if err := os.Setenv("CAPTURE_FLUSH_HELPER_MODE", "child"); err != nil {
			t.Fatal(err)
		}
		if err := startBackgroundFlush(transcriptcapture.RuntimeClaudeCode); err != nil {
			t.Fatal(err)
		}
		readCaptureFlushTestFile(t, filepath.Join(directory, "child-ready"))
		if err := syscall.Kill(-syscall.Getpgrp(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		t.Fatal("parent survived its process-group hangup")
	}
	separator := -1
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || strings.Join(os.Args[separator+1:], " ") != "transcript flush --runtime claude-code" {
		t.Fatalf("unexpected child argv: %v", os.Args)
	}
	nullInfo, err := os.Stat(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		info, err := stream.Stat()
		if err != nil || !os.SameFile(info, nullInfo) {
			t.Fatalf("child stream %s is not directly attached to %s", stream.Name(), os.DevNull)
		}
	}
	switch mode {
	case "child":
		if os.Getenv(captureDetachedFlushEnv) != "1" {
			t.Fatal("detached flusher flag is missing")
		}
		sid, err := unix.Getsid(0)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := json.Marshal(captureFlushProcessIdentity{PID: os.Getpid(), PGID: syscall.Getpgrp(), SID: sid})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "child-ready"), identity, 0o600); err != nil {
			t.Fatal(err)
		}
		readCaptureFlushTestFile(t, filepath.Join(directory, "parent-exited"))
		if err := os.WriteFile(filepath.Join(directory, "child-survived"), []byte("yes"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "stall":
		if os.Getenv(captureDetachedFlushEnv) != "" {
			t.Fatal("foreground child inherited detached flusher flag")
		}
		if err := os.WriteFile(filepath.Join(directory, "stall-ready"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Second)
		t.Fatal("stalled foreground child was not terminated")
	case "record":
		path := filepath.Join(directory, "foreground-observed")
		if os.Getenv(captureDetachedFlushEnv) == "" {
			if err := os.WriteFile(path, []byte("foreground\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if os.Getenv("CAPTURE_FLUSH_FAIL_FOREGROUND") == "true" {
				os.Exit(17)
			}
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "detached-observed"), append(raw, []byte("detached\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unexpected child mode %q", mode)
	}
}
