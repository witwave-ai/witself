//go:build linux

package localcell

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// The leader stays locked in initialization while the alternate mode executes
// from a second locked thread. The replacement waits for the parent's release,
// so an observed dead state cannot be explained by an actual completed exit.
func init() {
	if len(os.Args) != 3 || os.Args[1] != "localcell-exec-observation-fixture" {
		return
	}
	if os.Args[2] == "replacement" {
		if _, err := os.Stdout.Write([]byte{'R'}); err != nil {
			os.Exit(1)
		}
		var permit [1]byte
		if _, err := io.ReadFull(os.Stdin, permit[:]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	runtime.LockOSThread()
	run := func() {
		runtime.LockOSThread()
		marker := byte('N')
		if syscall.Gettid() == os.Getpid() {
			marker = 'L'
		}
		if _, err := os.Stdout.Write([]byte{marker}); err != nil {
			os.Exit(1)
		}
		var permit [1]byte
		if _, err := io.ReadFull(os.Stdin, permit[:]); err != nil {
			os.Exit(1)
		}
		if err := syscall.Exec(os.Args[0], []string{os.Args[0], os.Args[1], "replacement"}, os.Environ()); err != nil {
			os.Exit(1)
		}
	}
	switch os.Args[2] {
	case "leader":
		run()
	case "nonleader":
		go run()
		select {}
	default:
		os.Exit(1)
	}
}

// This is a bounded mechanism regression, not a probabilistic failure-rate
// assertion. Both modes must preserve their creation identity and pending Wait;
// only leader exec promises no false-dead observation. A zero nonleader count is
// valid when scheduling happens not to expose the brief kernel transition.
func TestLinuxExecObservationPreservesLiveIdentity(t *testing.T) {
	const launches = 8
	for _, mode := range []string{"leader", "nonleader"} {
		t.Run(mode, func(t *testing.T) {
			transitions := 0
			for range launches {
				if execObservationLaunch(t, mode) {
					transitions++
				}
			}
			if mode == "leader" && transitions != 0 {
				t.Fatal("leader exec produced a false dead observation")
			}
			t.Logf("launches=%d recovered_dead_observations=%d", launches, transitions)
		})
	}
}

func execObservationLaunch(t *testing.T, mode string) bool {
	t.Helper()
	// Each launch has one hang guard; the total bound scales with launch count.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "localcell-exec-observation-fixture", mode)
	cmd.Env = []string{
		"GORACE=atexit_sleep_ms=0",
		"WITSELF_HOME=" + t.TempDir(),
		"DSH_HOME=" + t.TempDir(),
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("exec observation input unavailable")
	}
	defer func() { _ = input.Close() }()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("exec observation output unavailable")
	}
	defer func() { _ = output.Close() }()
	if err := cmd.Start(); err != nil {
		t.Fatal("exec observation child did not start")
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	defer func() {
		_ = cmd.Process.Kill()
		<-done
	}()
	var marker [1]byte
	if _, err := io.ReadFull(output, marker[:]); err != nil {
		t.Fatal("exec observation mode unavailable")
	}
	if (mode == "leader" && marker[0] != 'L') || (mode == "nonleader" && marker[0] != 'N') {
		t.Fatal("exec observation used the wrong thread class")
	}
	before, alive, err := linuxProcessStartTicks(cmd.Process.Pid)
	if err != nil || !alive {
		t.Fatal("exec observation initial identity unavailable")
	}
	replacement := make(chan bool, 1)
	go func() {
		var ready [1]byte
		_, err := io.ReadFull(output, ready[:])
		replacement <- err == nil && ready[0] == 'R'
	}()
	if _, err := input.Write([]byte{1}); err != nil {
		t.Fatal("exec observation permission failed")
	}
	sawDead, replaced := false, false
	for {
		select {
		case <-ctx.Done():
			t.Fatal("exec observation replacement did not become ready")
		case <-done:
			t.Fatal("exec observation child exited before release")
		case ok := <-replacement:
			if !ok {
				t.Fatal("exec observation replacement marker unavailable")
			}
			replaced = true
		default:
		}
		current, live, err := linuxProcessStartTicks(cmd.Process.Pid)
		if err != nil {
			t.Fatal("exec observation process inspection failed")
		}
		sawDead = sawDead || !live
		if replaced && live {
			if current != before {
				t.Fatal("exec observation changed the creation identity")
			}
			select {
			case <-done:
				t.Fatal("exec observation wait completed before release")
			default:
			}
			break
		}
	}
	if _, err := input.Write([]byte{1}); err != nil {
		t.Fatal("exec observation release failed")
	}
	select {
	case <-done:
		if waitErr != nil {
			t.Fatal("exec observation child did not exit cleanly")
		}
	case <-ctx.Done():
		t.Fatal("exec observation child did not finish after release")
	}
	return sawDead
}
