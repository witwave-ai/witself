//go:build darwin || linux || windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestIntegrationOperationLockSerializesAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "operation.lock")
	const busy = "another test operation is already running"

	firstRelease, err := acquireIntegrationOperationLock(path, "test", busy)
	if err != nil {
		t.Fatal(err)
	}
	if release, err := acquireIntegrationOperationLock(path, "test", busy); err == nil {
		release()
		t.Fatal("second operation acquired the live lock")
	} else if err.Error() != busy {
		t.Fatalf("second lock error = %v", err)
	}
	firstRelease()
	firstRelease()

	thirdRelease, err := acquireIntegrationOperationLock(path, "test", busy)
	if err != nil {
		t.Fatalf("lock remained held after release: %v", err)
	}
	thirdRelease()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("operation lock mode = %v", info.Mode())
	}
}

func TestIntegrationOperationLockSerializesAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operation.lock")
	const busy = "another subprocess test operation is already running"
	release, err := acquireIntegrationOperationLock(path, "subprocess test", busy)
	if err != nil {
		t.Fatal(err)
	}
	runIntegrationOperationLockHelper(t, path, busy, "busy")
	release()
	runIntegrationOperationLockHelper(t, path, busy, "acquired")
}

func TestIntegrationOperationLockSubprocessHelper(t *testing.T) {
	if os.Getenv("WITSELF_OPERATION_LOCK_HELPER") != "1" {
		return
	}
	path := os.Getenv("WITSELF_OPERATION_LOCK_PATH")
	busy := os.Getenv("WITSELF_OPERATION_LOCK_BUSY")
	expect := os.Getenv("WITSELF_OPERATION_LOCK_EXPECT")
	release, err := acquireIntegrationOperationLock(path, "subprocess test", busy)
	switch expect {
	case "busy":
		if err == nil {
			release()
			t.Fatal("subprocess acquired a live integration operation lock")
		}
		if err.Error() != busy {
			t.Fatalf("subprocess lock error = %v", err)
		}
	case "acquired":
		if err != nil {
			t.Fatalf("subprocess did not acquire the released integration operation lock: %v", err)
		}
		release()
	default:
		t.Fatalf("unknown subprocess lock expectation %q", expect)
	}
}

func runIntegrationOperationLockHelper(t *testing.T, path, busy, expect string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestIntegrationOperationLockSubprocessHelper$")
	command.Env = append(
		os.Environ(),
		"WITSELF_OPERATION_LOCK_HELPER=1",
		"WITSELF_OPERATION_LOCK_PATH="+path,
		"WITSELF_OPERATION_LOCK_BUSY="+busy,
		"WITSELF_OPERATION_LOCK_EXPECT="+expect,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("integration operation lock helper failed: %v\n%s", err, output)
	}
}

func TestIntegrationOperationLockRejectsUnsafeShapes(t *testing.T) {
	t.Run("relative path", func(t *testing.T) {
		if release, err := acquireIntegrationOperationLock("relative.lock", "test", "busy"); err == nil {
			release()
			t.Fatal("relative integration lock path succeeded")
		}
	})

	t.Run("directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "operation.lock")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if release, err := acquireIntegrationOperationLock(path, "test", "busy"); err == nil {
			release()
			t.Fatal("directory integration lock path succeeded")
		}
	})

	t.Run("hard link", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "foreign")
		path := filepath.Join(root, "operation.lock")
		if err := os.WriteFile(target, []byte("foreign\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(target, path); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		if release, err := acquireIntegrationOperationLock(path, "test", "busy"); err == nil {
			release()
			t.Fatal("hard-linked integration lock path succeeded")
		}
		raw, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != "foreign\n" {
			t.Fatalf("hard-link target changed to %q", raw)
		}
	})
}

func TestProviderOperationLocksSerializeOnSupportedPlatforms(t *testing.T) {
	t.Run("generic provider", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
		t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
		assertProviderOperationLockSerializes(
			t,
			func() (func(), error) {
				return acquireRuntimeIntegrationOperationLock(transcriptcapture.RuntimeCodex)
			},
			"another Codex install, uninstall, or routing refresh is already running",
		)
	})

	t.Run("openclaw", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
		for _, key := range append([]string{
			"OPENCLAW_CONFIG_PATH", "OPENCLAW_STATE_DIR", "OPENCLAW_PROFILE",
		}, openClawUnsupportedSelectorEnvironment...) {
			t.Setenv(key, "")
		}
		assertProviderOperationLockSerializes(
			t,
			func() (func(), error) {
				return acquireRuntimeIntegrationOperationLock(transcriptcapture.RuntimeOpenClaw)
			},
			"another OpenClaw install, uninstall, or routing refresh is already running",
		)
	})

	t.Run("copilot", func(t *testing.T) {
		home := t.TempDir()
		root := filepath.Join(home, "copilot-home")
		t.Setenv("COPILOT_HOME", root)
		t.Setenv("WITSELF_HOME", filepath.Join(home, "witself-home"))
		assertProviderOperationLockSerializes(
			t,
			acquireCopilotOperationLock,
			"another GitHub Copilot install, uninstall, or routing refresh is already running",
		)
	})

	t.Run("antigravity", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "home")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
		t.Setenv("HOMEDRIVE", "")
		t.Setenv("HOMEPATH", "")
		assertProviderOperationLockSerializes(
			t,
			acquireAntigravityOperationLock,
			"another Antigravity install or uninstall is already running",
		)
	})
}

func TestRuntimeOperationLockSerializesDurableBindingAcrossProviderRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WITSELF_HOME", filepath.Join(home, ".witself"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex-a"))

	release, err := acquireRuntimeIntegrationOperationLock(transcriptcapture.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// A different provider selector still targets the same persisted integration
	// record under WITSELF_HOME and therefore must contend on the durable lock.
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex-b"))
	if secondRelease, err := acquireRuntimeIntegrationOperationLock(transcriptcapture.RuntimeCodex); err == nil {
		secondRelease()
		t.Fatal("different provider roots raced the same durable integration binding")
	} else if !strings.Contains(err.Error(), "another Codex install, uninstall, or routing refresh is already running") {
		t.Fatalf("second runtime lock error = %v", err)
	}
}

func assertProviderOperationLockSerializes(t *testing.T, acquire func() (func(), error), busy string) {
	t.Helper()
	firstRelease, err := acquire()
	if err != nil {
		t.Fatal(err)
	}
	if release, err := acquire(); err == nil {
		release()
		t.Fatal("second provider operation acquired the live lock")
	} else if !strings.Contains(err.Error(), busy) {
		t.Fatalf("second provider lock error = %v", err)
	}
	firstRelease()
	thirdRelease, err := acquire()
	if err != nil {
		t.Fatalf("provider lock remained held after release: %v", err)
	}
	thirdRelease()
}
