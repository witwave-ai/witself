package localcell

import (
	"context"
	"errors"
	"io"
	"net/http/httptrace"
	"os"
	"os/exec"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A separate host lets Windows observe the exit of its retained process handle.
func TestServiceRemovalRetriesDeferredStop(t *testing.T) {
	const hostArg = "removal-stop-test-host"
	if os.Args[len(os.Args)-1] == hostArg {
		root, err := os.Getwd()
		if err != nil {
			t.Fatal("cannot locate the removal host fixture")
		}
		c, err := New(Config{Root: root, Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal("cannot open the removal host fixture")
		}
		w := serviceWorkerOnCell(t, c, "health", false)
		w.ready(t)
		w.blockTick(t)
		if _, err := io.WriteString(os.Stdout, "+"); err != nil {
			t.Fatal("cannot announce the held removal tick")
		}
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal("cannot await the removal tick release")
		}
		w.free()
		if err := <-w.done; err != nil {
			t.Fatal("the removal host failed during exit")
		}
		return
	}
	c := serviceCancellationReceiptCell(t)
	d := prepareSupervisorTest(t, c)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.Payload.Host, "-test.run=^TestServiceRemovalRetriesDeferredStop$", hostArg)
	cmd.Dir = c.cfg.Root
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("cannot create the removal tick release pipe")
	}
	output, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		t.Fatal("cannot start the removal host process")
	}
	wait := sync.OnceValue(cmd.Wait)
	t.Cleanup(func() { cancel(); _ = wait() })
	var ready [1]byte
	if _, err := io.ReadFull(output, ready[:]); err != nil || ready[0] != '+' {
		t.Fatal("the removal host did not reach the held tick")
	}
	// Until the tick is released every removal request is its pre-exit stop.
	var responses atomic.Int32
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() {
		if responses.Add(1) == 2 {
			_ = input.Close()
		}
	}}
	if err := c.Remove(httptrace.WithClientTrace(ctx, trace), RemoveOptions{}); err != nil {
		t.Fatal("removal failed after the tick deferred its stop")
	}
	if responses.Load() < 2 {
		t.Fatal("removal did not resend the deferred stop")
	}
	if wait() != nil {
		t.Fatal("removal did not leave a cleanly exited host process")
	}
	if held, err := c.serviceHostLeaseHeld(); err != nil || held {
		t.Fatal("removal completed with the host lease still held")
	}
}

func TestServiceRemovalCancelsDeferredStop(t *testing.T) {
	w := serviceWorkerStart(t, "health", false)
	w.ready(t)
	w.blockTick(t)
	records := func() []string {
		var result []string
		for _, name := range []string{"manifest.json", "service/descriptor.json", "service/lease.json", "service/supervisor.json"} {
			data, err := os.ReadFile(w.cell.path(name))
			if err != nil {
				t.Fatal("a held-tick removal record was missing")
			}
			result = append(result, string(data))
		}
		return result
	}
	before := records()
	ctx, cancel := context.WithTimeout(t.Context(), w.cell.cfg.Timeout)
	defer cancel()
	var responses atomic.Int32
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() {
		if responses.Add(1) == 2 {
			cancel()
		}
	}}
	err := w.cell.Remove(httptrace.WithClientTrace(ctx, trace), RemoveOptions{Purge: true})
	if ctx.Err() != context.Canceled {
		t.Fatal("removal did not wait for cancellation while the stop was deferred")
	}
	var refusal *serviceControlRefusal
	if !errors.As(err, &refusal) || refusal.step != "host_exit:drain" {
		t.Fatal("canceling a deferred removal stop lost the drain refusal")
	}
	status, err := w.cell.Control(t.Context(), "observe")
	if err != nil || status.State != "running" || !status.WorkInFlight {
		t.Fatal("canceling a deferred removal stop did not preserve the running host")
	}
	if !slices.Equal(before, records()) {
		t.Fatal("canceling a deferred removal stop changed durable records")
	}
}

func TestServiceRemovalRefusesStopOnce(t *testing.T) {
	w := serviceWorkerStart(t, "", false)
	w.ready(t)
	// The host cannot persist stopped intent while another owner holds the lock.
	lock, err := w.cell.own(false)
	if err != nil {
		t.Fatal("cannot hold the removal stop's operation lock")
	}
	defer func() { _ = lock.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), w.cell.cfg.Timeout)
	defer cancel()
	var responses atomic.Int32
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() {
		if responses.Add(1) > 1 {
			cancel()
		}
	}}
	err = w.cell.Remove(httptrace.WithClientTrace(ctx, trace), RemoveOptions{})
	var refusal *serviceControlRefusal
	if !errors.As(err, &refusal) || refusal.step != "host_exit:drain" {
		t.Fatal("a refused removal stop lost the drain refusal")
	}
	if responses.Load() != 1 {
		t.Fatal("removal did not send a non-deferred refused stop exactly once")
	}
}
