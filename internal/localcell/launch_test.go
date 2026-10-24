//go:build darwin || linux

package localcell

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func gatedLaunchFixture(t *testing.T) (*Cell, io.WriteCloser, string, <-chan error) {
	t.Helper()
	c, _ := unitCell(t)
	if err := c.validateRoot(false); err != nil {
		t.Fatal(err)
	}
	marker := c.path("tmp", "executed")
	binary := c.path("tmp", "marker-child")
	script := "#!/bin/sh\nprintf executed > '" + strings.ReplaceAll(marker, "'", "'\\''") + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd, err := childCommand(binary, c.cfg.Root, "server")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env, cmd.Dir = c.baseEnvironment(), c.cfg.Root
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	permit, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = permit.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = permit.Close()
		_ = cmd.Process.Kill()
	})
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		state, err := c.processState("server")
		if err == nil && state.PID == cmd.Process.Pid && state.Alive && state.Matches {
			break
		}
		select {
		case <-done:
			t.Fatal("launch helper exited before publishing its identity")
		case <-deadline.C:
			t.Fatal("launch helper did not publish its identity before permission")
		case <-tick.C:
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("component executed before parent permission")
	}
	return c, permit, marker, done
}

func TestChildLaunchPublishesIdentityBeforeExec(t *testing.T) {
	_, permit, marker, done := gatedLaunchFixture(t)
	if _, err := permit.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("authorized component failed to execute")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("authorized component did not finish")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "executed" {
		t.Fatal("parent permission did not execute the intended component")
	}
}

func TestChildLaunchParentEOFPreventsExec(t *testing.T) {
	c, permit, marker, done := gatedLaunchFixture(t)
	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 125 {
			t.Fatal("launch helper did not refuse execution after parent EOF")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("launch helper survived parent EOF")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("component executed after its parent disappeared")
	}
	state, err := c.processState("server")
	if err != nil || state.Alive {
		t.Fatal("aborted helper retained a live process identity")
	}
}
