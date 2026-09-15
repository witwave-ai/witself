package transcriptcapture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const flushLeaseProcessMode = "WITSELF_FLUSH_LEASE_TEST_MODE"

// Exercise the kernel lease directly as well as the public lock: a working
// legacy PID marker must not hide a broken cross-process kernel lock.
func TestCapturePlatformFlushLeaseProcessExclusion(t *testing.T) {
	for _, mode := range []string{"lease", "flush"} {
		t.Run(mode, func(t *testing.T) {
			home, dir := flushLeaseProcessFixture(t)
			owner := startFlushLeaseProcess(t, home, mode)
			owner.requireAcquired(t, true)
			before, err := os.Stat(filepath.Join(dir, ".flush.lease"))
			if err != nil {
				t.Fatal(err)
			}
			contender := startFlushLeaseProcess(t, home, mode)
			contender.requireAcquired(t, false)
			contender.stop(t, false)
			owner.stop(t, false)
			successor := startFlushLeaseProcess(t, home, mode)
			successor.requireAcquired(t, true)
			after, err := os.Stat(filepath.Join(dir, ".flush.lease"))
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("lease inode changed across owners: %v", err)
			}
			successor.stop(t, false)
		})
	}
}

func TestCapturePlatformFlushLeaseProcessRecoversAfterOwnerDeath(t *testing.T) {
	for _, mode := range []string{"lease", "flush"} {
		t.Run(mode, func(t *testing.T) {
			home, dir := flushLeaseProcessFixture(t)
			owner := startFlushLeaseProcess(t, home, mode)
			owner.requireAcquired(t, true)
			before, err := os.Stat(filepath.Join(dir, ".flush.lease"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "flush" {
				requireFlushLeaseMarkerPID(t, dir, owner.cmd.Process.Pid)
			}
			// Kill, rather than closing stdin, bypasses both release callbacks.
			owner.stop(t, true)
			if mode == "flush" {
				requireFlushLeaseMarkerPID(t, dir, owner.cmd.Process.Pid)
			}
			successor := startFlushLeaseProcess(t, home, mode)
			successor.requireAcquired(t, true)
			after, err := os.Stat(filepath.Join(dir, ".flush.lease"))
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("lease inode changed after owner death: %v", err)
			}
			if mode == "flush" {
				requireFlushLeaseMarkerPID(t, dir, successor.cmd.Process.Pid)
			}
			successor.stop(t, false)
		})
	}
}

func TestCapturePlatformFlushLeaseProcessHonorsLiveLegacyOwner(t *testing.T) {
	home, dir := flushLeaseProcessFixture(t)
	legacy := startFlushLeaseProcess(t, home, "legacy")
	legacy.requireAcquired(t, true)
	marker, err := os.Stat(filepath.Join(dir, ".flush.lock"))
	if err != nil {
		t.Fatal(err)
	}
	contender := startFlushLeaseProcess(t, home, "flush")
	contender.requireAcquired(t, false)
	contender.stop(t, false)
	requireFlushLeaseMarkerPID(t, dir, legacy.cmd.Process.Pid)
	after, err := os.Stat(filepath.Join(dir, ".flush.lock"))
	if err != nil || !os.SameFile(marker, after) {
		t.Fatalf("refused contender replaced live legacy marker: %v", err)
	}
	// A refusal for the legacy marker must release the newly acquired lease.
	lease := startFlushLeaseProcess(t, home, "lease")
	lease.requireAcquired(t, true)
	lease.stop(t, false)
	legacy.stop(t, true)
	requireFlushLeaseMarkerPID(t, dir, legacy.cmd.Process.Pid)
	successor := startFlushLeaseProcess(t, home, "flush")
	successor.requireAcquired(t, true)
	requireFlushLeaseMarkerPID(t, dir, successor.cmd.Process.Pid)
	successor.stop(t, false)
}

func TestCapturePlatformFlushLeaseProcessRepeatedReleasePreservesNewOwner(t *testing.T) {
	for _, mode := range []string{"lease", "flush"} {
		t.Run(mode, func(t *testing.T) {
			home, dir := flushLeaseProcessFixture(t)
			var release func()
			var acquired bool
			var err error
			if mode == "lease" {
				release, acquired, err = acquireFlushLease(dir)
			} else {
				release, acquired, err = AcquireFlushLock(RuntimeClaudeCode)
			}
			if err != nil || !acquired {
				t.Fatalf("first owner: acquired=%t, err=%v", acquired, err)
			}
			t.Cleanup(release)
			release()
			owner := startFlushLeaseProcess(t, home, mode)
			owner.requireAcquired(t, true)
			release()
			if mode == "flush" {
				requireFlushLeaseMarkerPID(t, dir, owner.cmd.Process.Pid)
			}
			// Bypass the legacy marker so it cannot mask an accidentally released
			// kernel lease, then check the public API independently as well.
			for _, contenderMode := range []string{"lease", "flush"} {
				contender := startFlushLeaseProcess(t, home, contenderMode)
				contender.requireAcquired(t, false)
				contender.stop(t, false)
			}
			owner.stop(t, false)
		})
	}
}

func flushLeaseProcessFixture(t *testing.T) (home, dir string) {
	t.Helper()
	home = t.TempDir()
	for key, path := range map[string]string{
		"HOME": home, "USERPROFILE": home,
		"WITSELF_HOME": filepath.Join(home, "witself"),
		"DSH_HOME":     filepath.Join(home, "dsh"),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, path)
	}
	var err error
	dir, err = outboxDir(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return home, dir
}

func requireFlushLeaseMarkerPID(t *testing.T, dir string, pid int) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".flush.lock"))
	if err != nil || string(raw) != strconv.Itoa(pid)+"\n" {
		t.Fatalf("legacy marker does not identify expected owner: %v", err)
	}
}

type flushLeaseProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	done     chan struct{}
	waitErr  error // Written before closing done; read only after done.
	acquired bool
}

type flushLeaseProcessResult struct {
	Acquired bool   `json:"acquired"`
	Error    string `json:"error,omitempty"`
}

func startFlushLeaseProcess(t *testing.T, home, mode string) *flushLeaseProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready.json")
	cmd := exec.Command(executable, "-test.run=^TestCapturePlatformFlushLeaseProcessHelper$", "-test.timeout=1m")
	cmd.Env = []string{
		flushLeaseProcessMode + "=" + mode,
		"WITSELF_FLUSH_LEASE_TEST_READY=" + ready,
		"HOME=" + home, "USERPROFILE=" + home,
		"WITSELF_HOME=" + filepath.Join(home, "witself"),
		"DSH_HOME=" + filepath.Join(home, "dsh"),
		"TMPDIR=" + home, "TMP=" + home, "TEMP=" + home,
	}
	// Windows needs these OS paths; no application credentials or configuration
	// environment is inherited by the fixture process.
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	p := &flushLeaseProcess{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-p.done:
			return
		default:
		}
		_ = cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			t.Error("flush lease helper cleanup did not reap the owned process")
		}
	})
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	exited := false
	for {
		if raw, err := os.ReadFile(ready); err == nil {
			var result flushLeaseProcessResult
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			if result.Error != "" {
				t.Fatalf("flush lease helper: %s", result.Error)
			}
			p.acquired = result.Acquired
			return p
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if exited {
			t.Fatalf("flush lease helper exited before reporting acquisition: %v", p.waitErr)
		}
		select {
		case <-p.done:
			exited = true
		case <-deadline.C:
			t.Fatal("flush lease helper did not report acquisition")
		case <-ticker.C:
		}
	}
}

func (p *flushLeaseProcess) requireAcquired(t *testing.T, want bool) {
	t.Helper()
	if p.acquired != want {
		t.Fatalf("subprocess acquired=%t, want %t", p.acquired, want)
	}
}

func (p *flushLeaseProcess) stop(t *testing.T, force bool) {
	t.Helper()
	if force {
		if err := p.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
	} else {
		_ = p.stdin.Close()
	}
	select {
	case <-p.done:
		if force {
			var exitErr *exec.ExitError
			if !errors.As(p.waitErr, &exitErr) {
				t.Fatalf("killed owner did not report forced exit: %v", p.waitErr)
			}
		} else if p.waitErr != nil {
			t.Fatalf("flush lease helper exit: %v", p.waitErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("flush lease helper did not exit")
	}
}

// This helper runs only in the exact re-executed test binary. Holding stdin
// open retains ownership; closing it releases normally, while Kill skips all
// Go defers and leaves the legacy PID marker for the next owner to reclaim.
func TestCapturePlatformFlushLeaseProcessHelper(t *testing.T) {
	mode := os.Getenv(flushLeaseProcessMode)
	if mode == "" {
		t.Skip("subprocess helper")
	}
	dir, err := outboxDir(RuntimeClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	var release func()
	var acquired bool
	switch mode {
	case "lease":
		release, acquired, err = acquireFlushLease(dir)
	case "flush":
		release, acquired, err = AcquireFlushLock(RuntimeClaudeCode)
	case "legacy":
		path := filepath.Join(dir, ".flush.lock")
		var file *os.File
		file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, writeErr := fmt.Fprintf(file, "%d\n", os.Getpid())
			err = errors.Join(writeErr, file.Close())
			acquired = err == nil
			release = func() { _ = os.Remove(path) }
		}
	default:
		t.Fatal("unknown flush lease helper mode")
	}
	if release != nil {
		defer release()
	}
	result := flushLeaseProcessResult{Acquired: acquired}
	if err != nil {
		result.Error = err.Error()
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	ready := os.Getenv("WITSELF_FLUSH_LEASE_TEST_READY")
	if strings.TrimSpace(ready) == "" {
		t.Fatal("missing helper readiness path")
	}
	if err := os.WriteFile(ready+".tmp", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(ready+".tmp", ready); err != nil {
		t.Fatal(err)
	}
	if acquired {
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
	}
}
