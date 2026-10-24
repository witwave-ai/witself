//go:build darwin || linux || windows

package localcell

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestProcessIdentityHelperProcess(_ *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "localcell-process-helper" {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestProcessIdentityStableAndExited(t *testing.T) {
	started, err := processStartedAt(os.Getpid())
	if err != nil || started.IsZero() || started.After(time.Now().Add(time.Second)) {
		t.Fatalf("current process start time unavailable: %v", err)
	}
	again, err := processStartedAt(os.Getpid())
	if err != nil || !again.Equal(started) {
		t.Fatal("current process start time changed between reads")
	}
	first, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive || first == "" {
		t.Fatalf("current process identity unavailable: alive=%t err=%v", alive, err)
	}
	second, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive || first != second {
		t.Fatal("current process identity changed between reads")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessIdentityHelperProcess$", "--", "localcell-process-helper")
	child.Env = []string{}
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatal("helper did not become ready")
	}
	identity, alive, err := processIdentity(child.Process.Pid)
	if err != nil || !alive || identity == "" {
		t.Fatalf("child process identity unavailable: alive=%t err=%v", alive, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	_, alive, err = processIdentity(child.Process.Pid)
	if err != nil || alive {
		t.Fatalf("exited process still alive: alive=%t err=%v", alive, err)
	}
	if _, err := processStartedAt(child.Process.Pid); err == nil {
		t.Fatal("exited process start time accepted")
	}
}

func TestProcessIdentityRejectsInvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if _, alive, err := processIdentity(pid); err == nil || alive {
			t.Fatalf("invalid PID %d was accepted", pid)
		}
		if err := signalTerminate(pid); err == nil {
			t.Fatalf("signal accepted invalid PID %d", pid)
		}
		if _, err := processStartedAt(pid); err == nil {
			t.Fatalf("start time accepted invalid PID %d", pid)
		}
	}
}
