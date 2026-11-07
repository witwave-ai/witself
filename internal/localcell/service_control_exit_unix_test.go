//go:build darwin || linux

package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func hostExitSocketFixture(t *testing.T) (*Cell, chan serviceCommand) {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	c := shortServiceTestCell(t)
	d, err := c.ServiceDescriptor()
	if err != nil {
		t.Fatal("descriptor unavailable")
	}
	lease, err := c.acquireServiceLease(d)
	if err != nil {
		t.Fatal("lease unavailable")
	}
	t.Cleanup(func() { _ = lease.Close() })
	commands := make(chan serviceCommand)
	closeControl, err := c.serveServiceControl(commands)
	if err != nil {
		t.Fatal("socket unavailable")
	}
	t.Cleanup(closeControl)
	return c, commands
}

func hostExitRawRequest(t *testing.T, c *Cell, body []byte) (int, serviceHostExitResponse, string) {
	t.Helper()
	secret, err := c.readServiceControlSecret()
	if err != nil {
		t.Fatal("fixture authentication unavailable")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dialServiceControl(ctx) }}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: serviceControlBudget(c.cfg.Timeout, 4)}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://localcell/exit-host", bytes.NewReader(body))
	if err != nil {
		t.Fatal("cannot create fixture request")
	}
	request.Header.Set("Authorization", "Bearer "+string(secret))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("fixture request failed")
	}
	defer func() { _ = response.Body.Close() }()
	var receipt serviceHostExitResponse
	var detail string
	if response.StatusCode == http.StatusOK {
		if json.NewDecoder(response.Body).Decode(&receipt) != nil {
			t.Fatal("fixture response malformed")
		}
	} else {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		detail = strings.TrimSpace(string(body))
	}
	return response.StatusCode, receipt, detail
}

// Keep the healthy command path available for both accepted and rejected requests.
// The atomic count is observed after the HTTP response, which follows delivery.
func hostExitHealthyResponder(t *testing.T, commands <-chan serviceCommand) *atomic.Int32 {
	t.Helper()
	var deliveries atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case command := <-commands:
				deliveries.Add(1)
				command.reply <- serviceReply{}
				select {
				case <-command.delivered:
				case <-ctx.Done():
					return
				}
				command.completed <- "drained"
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return &deliveries
}

func TestServiceHostExitEnvelopeUnix(t *testing.T) {
	for _, site := range []string{"installation", "generation", "pid", "boot", "challenge", "oversize", "unknown", "trailing"} {
		t.Run(site, func(t *testing.T) {
			c, commands := hostExitSocketFixture(t)
			deliveries := hostExitHealthyResponder(t, commands)
			envelope, _, err := c.newHostExitEnvelope()
			if err != nil {
				t.Fatal("cannot prepare envelope")
			}
			bad := envelope
			switch site {
			case "installation":
				bad.InstallationID += "-other"
			case "generation":
				bad.Generation += "-other"
			case "pid":
				bad.HostPID++
			case "boot":
				bad.BootID += "-other"
			case "challenge":
				bad.Challenge = "invalid"
			}
			body, _ := json.Marshal(bad)
			switch site {
			case "oversize":
				body = append(body, []byte(strings.Repeat(" ", 4097))...)
			case "unknown":
				body = append(body[:len(body)-1], []byte(`,"other":true}`)...)
			case "trailing":
				body = append(body, []byte(` {}`)...)
			}
			code, _, detail := hostExitRawRequest(t, c, body)
			if code != http.StatusConflict || detail != "service_control_refused: host_exit:envelope" {
				t.Fatal("invalid exit envelope was not refused at envelope boundary")
			}
			if deliveries.Load() != 0 {
				t.Fatal("envelope bypass reached downstream supervisor")
			}
			// The responder was healthy before the refusal, so a bypass fails
			// the envelope assertion rather than timing out downstream.
			body, _ = json.Marshal(envelope)
			code, receipt, _ := hostExitRawRequest(t, c, body)
			if deliveries.Load() != 1 || code != http.StatusOK || receipt.Disposition != "accepted" || receipt.Challenge != envelope.Challenge {
				t.Fatal("clean envelope did not reach acknowledged command")
			}
		})
	}
}

func TestServiceHostExitReplayedNonceUnix(t *testing.T) {
	c, commands := hostExitSocketFixture(t)
	envelope, _, err := c.newHostExitEnvelope()
	if err != nil {
		t.Fatal("cannot prepare envelope")
	}
	deliveries := hostExitHealthyResponder(t, commands)
	body, _ := json.Marshal(envelope)
	code, _, _ := hostExitRawRequest(t, c, body)
	if code != http.StatusOK || deliveries.Load() != 1 {
		t.Fatal("clean exit envelope refused")
	}
	code, _, detail := hostExitRawRequest(t, c, body)
	if code != http.StatusConflict || detail != "service_control_refused: host_exit:envelope" {
		t.Fatal("replayed exit challenge accepted")
	}
	if deliveries.Load() != 1 {
		t.Fatal("replay reached supervisor")
	}
}

func hostExitRunFixture(t *testing.T, stop func(context.Context) error) (*Cell, <-chan error, context.CancelFunc) {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	c := shortServiceTestCell(t)
	h, _, _, _, _ := supervisorTestHooks()
	h.ticks = make(chan time.Time)
	var stops atomic.Int32
	h.stop = func(ctx context.Context) error {
		if stops.Add(1) > 1 {
			return stop(ctx)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, SupervisorPolicy{hooks: &h}) }()
	readyCtx, readyCancel := context.WithTimeout(t.Context(), serviceControlBudget(c.cfg.Timeout, 3))
	defer readyCancel()
	for {
		status, err := c.Control(readyCtx, "observe")
		if err == nil && status.State == "running" {
			break
		}
		select {
		case <-readyCtx.Done():
			cancel()
			t.Fatal("host did not become ready")
		case <-time.After(time.Millisecond):
		}
	}
	return c, done, cancel
}

func TestServiceHostExitAcceptedThenExitedUnix(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	probe, phase := make(chan struct{}), make(chan bool, 1)
	c, done, cancel := hostExitRunFixture(t, func(ctx context.Context) error {
		close(entered)
		select {
		case <-probe:
			// The test and active drain rendezvous after the ACK. A detached
			// drain must not discard the accepted request's cancellation bound.
			_, bounded := ctx.Deadline()
			phase <- bounded && ctx.Done() != nil && ctx.Err() == nil
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	defer cancel()
	envelope, lease, err := c.newHostExitEnvelope()
	if err != nil {
		t.Fatal("cannot prepare exit envelope")
	}
	observer, err := retainServiceExitHost(lease)
	if err != nil {
		t.Fatal("cannot retain host")
	}
	defer observer.close()
	if err := c.hostExitRequest(t.Context(), envelope); err != nil {
		t.Fatal("valid request did not ACK")
	}
	<-entered
	close(probe)
	if !<-phase {
		t.Fatal("active drain lost the accepted request cancellation bound")
	}
	select {
	case <-done:
		t.Fatal("ACK was confused with exited")
	default:
	}
	held, err := c.serviceHostLeaseHeld()
	if err != nil || !held {
		t.Fatal("lease released before child drain")
	}
	close(release)
	if err := c.waitServiceHostExit(t.Context(), lease, observer); err != nil {
		t.Fatal("completed exit not observed")
	}
	if err := <-done; err != nil {
		t.Fatal("graceful host exit failed")
	}
	descriptor, err := c.ServiceDescriptor()
	if err != nil || descriptor.DesiredState != "running" {
		t.Fatal("host restart changed running intent")
	}
	// Same-boot successor acquires the same stable lease, retaining running intent.
	h, _, _, _, _ := supervisorTestHooks()
	h.ticks = make(chan time.Time)
	restartCtx, restartCancel := context.WithCancel(t.Context())
	restartDone := make(chan error, 1)
	go func() { restartDone <- c.Run(restartCtx, SupervisorPolicy{hooks: &h}) }()
	for {
		status, err := c.Control(t.Context(), "observe")
		if err == nil && status.State == "running" {
			break
		}
		select {
		case err := <-restartDone:
			t.Fatalf("same-boot successor failed: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
	restartCancel()
	if err := <-restartDone; err != nil {
		t.Fatal("same-boot successor stop failed")
	}
}

func TestServiceHostExitRefusedDrainRetainsRecordsUnix(t *testing.T) {
	c, done, cancel := hostExitRunFixture(t, func(context.Context) error { return errors.New("synthetic unknown stop") })
	defer func() { cancel(); <-done }()
	before, err := os.ReadFile(c.path("manifest.json"))
	if err != nil {
		t.Fatal("cannot snapshot manifest")
	}
	if _, err := c.ExitServiceHost(t.Context()); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("unknown drain was not refused")
	}
	select {
	case <-done:
		t.Fatal("refused drain exited host")
	default:
	}
	after, err := os.ReadFile(c.path("manifest.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused drain deleted or rewrote records")
	}
}

func TestServiceRemoveWaitsForObservedHostExitUnix(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var drains atomic.Int32
	c, done, cancel := hostExitRunFixture(t, func(ctx context.Context) error {
		if drains.Add(1) != 1 {
			return nil
		}
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	defer cancel()
	marker := c.path("logs", "keep")
	if os.WriteFile(marker, []byte("fixture"), 0o600) != nil {
		t.Fatal("cannot create disposable fixture")
	}
	removed := make(chan error, 1)
	go func() { removed <- c.Remove(t.Context(), RemoveOptions{}) }()
	<-entered
	descriptor, err := c.ServiceDescriptor()
	if err != nil || descriptor.DesiredState != "stopped" {
		t.Fatal("removal did not persist stopped before drain")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("removal deleted records before host exit")
	}
	select {
	case <-removed:
		t.Fatal("removal completed before host exit")
	default:
	}
	close(release)
	if err := <-removed; err != nil {
		t.Fatalf("removal failed after observed host exit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal("host failed graceful removal")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removal did not clean disposable data")
	}
}

func TestServiceHostExitCallerCancellationAfterACKUnix(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	c, done, stopHost := hostExitRunFixture(t, func(ctx context.Context) error {
		if attempts.Add(1) != 1 {
			return nil
		}
		close(entered)
		select {
		case <-ctx.Done():
			close(canceled)
			return ctx.Err()
		case <-release:
			// Cleanup can release a detached drain without satisfying canceled.
			return context.Canceled
		}
	})
	hostExited := false
	defer func() {
		close(release)
		stopHost()
		if !hostExited {
			<-done
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	status, err := c.Control(ctx, "exit-host")
	if err != nil || status.HostExit != "accepted" {
		cancel()
		t.Fatal("request did not reach ACK")
	}
	<-entered
	cancel()
	// Use the fixture's readiness budget (three configured cell timeouts),
	// well before the independent whole-tree shutdown deadline can fire.
	watchdog := time.NewTimer(serviceControlBudget(c.cfg.Timeout, 3))
	defer watchdog.Stop()
	select {
	case <-canceled:
	case <-watchdog.C:
		t.Fatal("accepted drain did not inherit caller cancellation")
	}
	select {
	case <-done:
		hostExited = true
		t.Fatal("caller cancellation authorized host exit")
	default:
	}
	// A successful fault write is the synchronization boundary after cancellation.
	for {
		d, err := c.ServiceDescriptor()
		if err == nil && d.Faulted {
			break
		}
		select {
		case <-t.Context().Done():
			t.Fatal("canceled drain did not fault")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestServiceHostExitForcedObservedUnix(t *testing.T) {
	c, done, cancel := hostExitRunFixture(t, func(context.Context) error { return ErrServiceShutdownForced })
	defer cancel()
	status, err := c.ExitServiceHost(t.Context())
	if status.HostExit != "exited" || !errors.Is(err, ErrServiceShutdownForced) {
		t.Fatal("observed forced drain earned a graceful exit receipt")
	}
	if err := <-done; !errors.Is(err, ErrServiceShutdownForced) {
		t.Fatal("host discarded aggregate forced failure")
	}
}

func TestServiceRemoveForcedDrainRetainsRecordsUnix(t *testing.T) {
	c, done, cancel := hostExitRunFixture(t, func(context.Context) error { return ErrServiceShutdownForced })
	defer func() { cancel(); <-done }()
	marker := c.path("logs", "retain")
	if os.WriteFile(marker, []byte("fixture"), 0o600) != nil {
		t.Fatal("cannot create retained fixture")
	}
	if err := c.Remove(t.Context(), RemoveOptions{}); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("forced stop did not refuse removal")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("forced stop deleted retained records")
	}
	select {
	case <-done:
		t.Fatal("forced stop failure unexpectedly exited host during removal")
	default:
	}
}

func TestServiceSupervisorWindowsStopFaultSuppressesDeferredDrain(t *testing.T) {
	for _, scenario := range []string{"stop", "caller-canceled", "health"} {
		t.Run(scenario, func(t *testing.T) {
			callerCanceled := scenario == "caller-canceled"
			t.Setenv("WITSELF_HOME", t.TempDir())
			t.Setenv("DSH_HOME", t.TempDir())
			c := shortServiceTestCell(t)
			h, _, _, _, _ := supervisorTestHooks()
			h.windowsShutdownPolicy = true
			ticks := make(chan time.Time)
			h.ticks = ticks
			var stops, starts, healths atomic.Int32
			h.start = func(context.Context) error { starts.Add(1); return nil }
			h.health = func(context.Context) error {
				if healths.Add(1) > 1 && scenario == "health" {
					return errors.New("synthetic unhealthy")
				}
				return nil
			}
			entered := make(chan struct{})
			h.stop = func(ctx context.Context) error {
				if stops.Add(1) == 1 {
					return nil
				}
				if callerCanceled {
					close(entered)
					<-ctx.Done()
					return ctx.Err()
				}
				return errors.New("synthetic unknown child")
			}
			hostCtx, stopHost := context.WithCancel(t.Context())
			defer stopHost()
			done := make(chan error, 1)
			go func() { done <- c.Run(hostCtx, SupervisorPolicy{hooks: &h}) }()
			for {
				status, err := c.Control(t.Context(), "observe")
				if err == nil && status.State == "running" {
					break
				}
				select {
				case <-done:
					t.Fatal("host failed before stop")
				case <-time.After(time.Millisecond):
				}
			}
			if scenario == "health" {
				ticks <- time.Now()
			} else {
				requestCtx, cancelRequest := context.WithCancel(t.Context())
				requestDone := make(chan error, 1)
				go func() { _, err := c.Control(requestCtx, "stop"); requestDone <- err }()
				if callerCanceled {
					<-entered
					cancelRequest()
				}
				if err := <-requestDone; err == nil {
					cancelRequest()
					t.Fatal("unsafe stop was acknowledged graceful")
				}
				cancelRequest()
			}
			// Polling the durable descriptor synchronizes the asynchronous cancellation.
			for {
				d, err := c.ServiceDescriptor()
				if err == nil && d.Faulted {
					break
				}
				select {
				case <-done:
					t.Fatal("host exited instead of faulting")
				case <-time.After(time.Millisecond):
				}
			}
			ticks <- time.Now()
			status, err := c.Control(t.Context(), "observe")
			if err != nil || status.State != "faulted" {
				t.Fatal("refused stop did not remain observable as faulted")
			}
			stopHost()
			if err := <-done; err != nil {
				t.Fatal("faulted host could not exit without another stop")
			}
			if stops.Load() != 2 || starts.Load() != 1 {
				t.Fatal("fresh tick or deferred context retried unsafe shutdown")
			}
		})
	}
}
