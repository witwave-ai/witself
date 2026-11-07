package localcell

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Host exit is a distinct authenticated transaction. An accepted response is
// only an acknowledgement; a caller must observe the retained host and lease.
type serviceHostExitEnvelope struct {
	InstallationID string `json:"installation_id"`
	Generation     string `json:"generation"`
	HostPID        int    `json:"host_pid"`
	BootID         string `json:"boot_id"`
	Challenge      string `json:"challenge"`
}
type serviceHostExitResponse struct {
	Disposition string `json:"disposition"`
	Challenge   string `json:"challenge"`
}
type serviceHostExitKey struct{}
type serviceHostExitRequests struct {
	mu   sync.Mutex
	seen map[string]bool
}

func hostExitRefused(step string) error { return &serviceControlRefusal{step: "host_exit:" + step} }

const serviceHostExitAdmissionTimeout = "service_control_refused: host_exit:admission"

// A timer's deadline can pass before its callback publishes Err. Check the
// selected ACK deadline with the same monotonic clock, preserving published
// cancellation first. An early transport failure is still a refusal.
func serviceHostExitAdmissionCause(admission, caller context.Context, now time.Time) error {
	if err := admission.Err(); err != nil {
		return err
	}
	if err := caller.Err(); err != nil {
		return err
	}
	if deadline, ok := admission.Deadline(); ok && !now.Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

// Preserve the failure boundary without exposing paths, process identities or
// transport error text to a caller's diagnostics.
type serviceHostExitError struct {
	err      error
	phase    string
	cause    string
	accepted bool
}

func (e *serviceHostExitError) Error() string {
	return "service host exit: " + e.phase + ": " + e.cause
}
func (e *serviceHostExitError) Unwrap() error { return e.err }
func (e *serviceHostExitError) ServiceHostExitDiagnostic() (phase, cause string, ackSeen bool) {
	return e.phase, e.cause, e.accepted
}

func hostExitFailure(phase string, accepted bool, err error) error {
	if err == nil {
		return nil
	}
	var recorded *serviceHostExitError
	if errors.As(err, &recorded) {
		return err
	}
	switch phase {
	case "identity_tree", "endpoint", "retain", "admission", "terminal", "lease_validate", "lease_lock", "observe", "drain":
	default:
		phase = "other"
	}
	cause := "other"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		cause = "deadline"
	case errors.Is(err, context.Canceled):
		cause = "canceled"
	case errors.Is(err, ErrServiceShutdownForced):
		cause = "forced"
	case runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(32)):
		cause = "sharing"
	case errors.Is(err, os.ErrNotExist):
		cause = "missing"
	case errors.Is(err, os.ErrPermission):
		cause = "privacy"
	case errors.Is(err, ErrServiceControlRefused):
		cause = "refused"
	}
	return &serviceHostExitError{err: err, phase: phase, cause: cause, accepted: accepted}
}

func validHostExitChallenge(challenge string) bool {
	if len(challenge) != 26 || challenge != strings.ToLower(challenge) {
		return false
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(challenge))
	return err == nil && len(decoded) == 16 && strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(decoded)) == challenge
}

func (c *Cell) serviceHostExitIdentity() (ServiceDescriptor, serviceLeaseRecord, error) {
	d, err := c.ServiceDescriptor()
	if err != nil {
		return d, serviceLeaseRecord{}, hostExitFailure("identity_tree", false, errors.Join(hostExitRefused("envelope"), err))
	}
	var lease serviceLeaseRecord
	if err := servicePrivateRecord(c.path("service", "lease.json")); err != nil {
		return d, lease, hostExitFailure("lease_validate", false, errors.Join(hostExitRefused("envelope"), err))
	}
	if err := readJSON(c.path("service", "lease.json"), &lease); err != nil {
		return d, lease, hostExitFailure("lease_validate", false, errors.Join(hostExitRefused("envelope"), err))
	}
	boot, err := serviceBootID()
	if err != nil || boot == "" || lease.Version != 1 || lease.BootID != boot || lease.InstallationID != d.InstallationID || lease.Owner != d.UID || lease.PID <= 1 || lease.StartIdentity == "" {
		return d, lease, hostExitRefused("envelope")
	}
	return d, lease, nil
}

func (c *Cell) validateHostExitEnvelope(envelope serviceHostExitEnvelope) error {
	d, lease, err := c.serviceHostExitIdentity()
	if err != nil {
		return err
	}
	identity, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive || lease.PID != os.Getpid() || lease.StartIdentity != identity ||
		envelope.InstallationID != d.InstallationID || envelope.Generation != d.Payload.Generation || envelope.HostPID != lease.PID || envelope.BootID != lease.BootID || !validHostExitChallenge(envelope.Challenge) {
		return hostExitRefused("envelope")
	}
	return nil
}

func (c *Cell) serveHostExit(w http.ResponseWriter, r *http.Request, commands chan<- serviceCommand, closed <-chan struct{}, requests *serviceHostExitRequests) {
	budgets := serviceHostExitBudgets(c.cfg.Timeout)
	ctx, cancel := context.WithTimeout(r.Context(), serviceHostExitPhaseBudget(r.Context(), budgets.transaction))
	defer cancel()
	admissionCtx, admissionCancel := context.WithTimeout(ctx, serviceHostExitPhaseBudget(ctx, budgets.admission))
	defer admissionCancel()
	deadline, _ := ctx.Deadline()
	if http.NewResponseController(w).SetWriteDeadline(deadline) != nil {
		http.Error(w, "service_control_refused: host_exit:envelope", http.StatusConflict)
		return
	}
	limited := &io.LimitedReader{R: r.Body, N: 4097}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var envelope serviceHostExitEnvelope
	if decoder.Decode(&envelope) != nil || decoder.Decode(new(any)) != io.EOF || limited.N == 0 || c.validateHostExitEnvelope(envelope) != nil {
		http.Error(w, "service_control_refused: host_exit:envelope", http.StatusConflict)
		return
	}
	requests.mu.Lock()
	replay := requests.seen[envelope.Challenge] || len(requests.seen) >= 1024
	if !replay {
		requests.seen[envelope.Challenge] = true
	}
	requests.mu.Unlock()
	if replay {
		http.Error(w, "service_control_refused: host_exit:envelope", http.StatusConflict)
		return
	}
	delivered := make(chan struct{})
	completed := make(chan string, 1)
	finalized := make(chan struct{})
	defer close(finalized)
	var deliveredOnce sync.Once
	deliver := func() { deliveredOnce.Do(func() { close(delivered) }) }
	command := serviceCommand{ctx: ctx, action: "exit-host", reply: make(chan serviceReply, 1), exit: &envelope, delivered: delivered, completed: completed, finalized: finalized}
	select {
	case commands <- command:
	case <-admissionCtx.Done():
		if admissionCtx.Err() == context.DeadlineExceeded {
			http.Error(w, serviceHostExitAdmissionTimeout, http.StatusRequestTimeout)
		}
		return
	case <-closed:
		return
	}
	defer deliver()
	select {
	case reply := <-command.reply:
		if reply.err != nil {
			http.Error(w, "service_control_refused: host_exit:drain", http.StatusConflict)
			return
		}
		ack, _ := json.Marshal(serviceHostExitResponse{Disposition: "accepted", Challenge: envelope.Challenge})
		finalShape, _ := json.Marshal(serviceHostExitResponse{Disposition: "drained", Challenge: envelope.Challenge})
		// Both lines have a fixed total length. This lets the client observe a
		// strict body EOF before Run closes its listener; chunked EOF would
		// race the host's deferred server.Close after the handler returns.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(ack)+len(finalShape)+2))
		if _, err := w.Write(append(ack, '\n')); err != nil {
			return
		}
		if flush, ok := w.(http.Flusher); ok {
			flush.Flush()
		}
		completionCtx, completionCancel := context.WithTimeout(ctx, serviceHostExitPhaseBudget(ctx, ServiceShutdownTimeout))
		defer completionCancel()
		completionDeadline, _ := completionCtx.Deadline()
		if http.NewResponseController(w).SetWriteDeadline(completionDeadline) != nil {
			return
		}
		deliver()
		select {
		case disposition := <-completed:
			final, _ := json.Marshal(serviceHostExitResponse{Disposition: disposition, Challenge: envelope.Challenge})
			if len(final) > len(finalShape) {
				return
			}
			for len(final) < len(finalShape) {
				final = append(final, ' ')
			}
			if _, err := w.Write(append(final, '\n')); err != nil {
				return
			}
			if flush, ok := w.(http.Flusher); ok {
				flush.Flush()
			}
		case <-completionCtx.Done():
		case <-closed:
		}
	case <-admissionCtx.Done():
		if admissionCtx.Err() == context.DeadlineExceeded {
			http.Error(w, serviceHostExitAdmissionTimeout, http.StatusRequestTimeout)
		}
	case <-closed:
	}
}

func (s *supervisor) acceptHostExit(ctx context.Context) (ServiceStatus, error) {
	envelope, ok := ctx.Value(serviceHostExitKey{}).(serviceHostExitEnvelope)
	if !ok || s.cell.validateHostExitEnvelope(envelope) != nil {
		return s.status, hostExitRefused("envelope")
	}
	if s.status.State == "faulted" || s.status.HostExit == "accepted" || s.status.HostExit == "refused" {
		return s.status, hostExitRefused("drain")
	}
	s.status.HostExit = "accepted"
	return s.status, nil
}

func (s *supervisor) finishHostExitCommand(hostCtx context.Context, command serviceCommand) bool {
	ctx, cancel := context.WithCancel(command.ctx)
	stop := context.AfterFunc(hostCtx, cancel)
	defer func() { stop(); cancel() }()
	ctx = context.WithValue(ctx, serviceControlHostContextKey{}, hostCtx)
	ctx = context.WithValue(ctx, serviceControlCommandKey{}, true)
	return s.finishHostExit(ctx, command.delivered)
}

// The HTTP acknowledgement is flushed before any drain. The drain belongs to
// the still-running host, whose cancellation remains a no-force boundary.
func (s *supervisor) finishHostExit(ctx context.Context, delivered <-chan struct{}) bool {
	if delivered == nil {
		return false
	}
	select {
	case <-delivered:
	case <-ctx.Done():
		return false
	}
	drainCtx, cancel := s.hooks.shutdownContext(ctx, serviceHostExitPhaseBudget(ctx, ServiceShutdownTimeout))
	defer cancel()
	err := retryServiceContention(drainCtx, func(ctx context.Context) error { return s.call(ctx, "stop", s.hooks.stop) })
	if !serviceStopPermitsHostExit(err) || drainCtx.Err() != nil {
		s.status.HostExit = "refused"
		_ = s.fault(context.WithoutCancel(ctx), "service_stop_refused")
		return false
	}
	s.running = false
	s.exitDrained = true
	s.exitDrainResult = err
	progress, journalErr := s.cell.readJournal()
	fmt.Fprintf(os.Stderr, "service_shutdown phase=control operation=none journal_pending=%t journal_read_ok=%t\n", progress.pending != nil || progress.tornTail, journalErr == nil)
	return true
}

type serviceHostExitSession struct {
	ctx         context.Context
	body        io.ReadCloser
	limited     *io.LimitedReader
	cancel      context.CancelFunc
	transport   *http.Transport
	challenge   string
	disposition string
	err         error
}

func readServiceHostExitLine(body io.Reader) (serviceHostExitResponse, error) {
	var receipt serviceHostExitResponse
	var line []byte
	one := make([]byte, 1)
	for len(line) <= 4096 {
		if _, err := io.ReadFull(body, one); err != nil {
			return receipt, hostExitRefused("envelope")
		}
		line = append(line, one[0])
		if one[0] == '\n' {
			break
		}
	}
	if len(line) > 4096 {
		return receipt, hostExitRefused("envelope")
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF {
		return receipt, hostExitRefused("envelope")
	}
	return receipt, nil
}
func (s *serviceHostExitSession) finish() {
	defer func() { _ = s.body.Close(); s.transport.CloseIdleConnections() }()
	reader := io.Reader(s.body)
	if s.limited != nil {
		reader = s.limited
	}
	receipt, err := readServiceHostExitLine(reader)
	if err != nil || receipt.Challenge != s.challenge || (s.limited != nil && s.limited.N == 0) {
		s.err = hostExitRefused("envelope")
		return
	}
	var trailing [1]byte
	if n, err := reader.Read(trailing[:]); n != 0 || err != io.EOF {
		s.err = hostExitRefused("envelope")
		return
	}
	s.disposition = receipt.Disposition
	switch receipt.Disposition {
	case "drained":
	case "forced":
		s.err = ErrServiceShutdownForced
	default:
		s.err = hostExitRefused("drain")
	}
}

func (c *Cell) startHostExitRequest(ctx context.Context, envelope serviceHostExitEnvelope) (*serviceHostExitSession, error) {
	if os.Getenv("WITSELF_RUNTIME") != "local" || serviceControlCapability() != nil || c.validateServiceControlEndpoint() != nil {
		return nil, hostExitRefused("envelope")
	}
	secret, err := c.readServiceControlSecret()
	if err != nil {
		return nil, hostExitRefused("envelope")
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, hostExitRefused("envelope")
	}
	budgets := serviceHostExitBudgets(c.cfg.Timeout)
	requestCtx, cancel := context.WithTimeout(ctx, serviceHostExitPhaseBudget(ctx, budgets.transaction))
	admission := serviceHostExitPhaseBudget(requestCtx, budgets.admission)
	ackCtx, ackCancel := context.WithTimeout(requestCtx, admission)
	defer ackCancel()
	stopACK := context.AfterFunc(ackCtx, cancel)
	defer stopACK()
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: admission, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dialServiceControl(ctx) }}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "http://localcell/exit-host", bytes.NewReader(body))
	if err != nil {
		cancel()
		transport.CloseIdleConnections()
		return nil, hostExitRefused("envelope")
	}
	request.Header.Set("Authorization", "Bearer "+string(secret))
	response, err := client.Do(request)
	if err != nil {
		cause := errors.Join(hostExitRefused("envelope"), serviceHostExitAdmissionCause(ackCtx, ctx, time.Now()))
		cancel()
		transport.CloseIdleConnections()
		return nil, hostExitFailure("admission", false, cause)
	}
	limited := &io.LimitedReader{R: response.Body, N: 4097}
	session := &serviceHostExitSession{body: response.Body, limited: limited, cancel: cancel, transport: transport, challenge: envelope.Challenge}
	refuse := func(step string, resultErr error) (*serviceHostExitSession, error) {
		cause := serviceHostExitAdmissionCause(ackCtx, ctx, time.Now())
		if cause == nil {
			cause = resultErr
		}
		cause = errors.Join(hostExitRefused(step), cause)
		cancel()
		_ = response.Body.Close()
		transport.CloseIdleConnections()
		return nil, hostExitFailure("admission", false, cause)
	}
	if response.StatusCode != http.StatusOK {
		var resultErr error
		if response.StatusCode == http.StatusRequestTimeout {
			data, readErr := io.ReadAll(limited)
			if readErr == nil && limited.N > 0 && string(data) == serviceHostExitAdmissionTimeout+"\n" {
				resultErr = context.DeadlineExceeded
			}
		}
		return refuse("drain", resultErr)
	}
	// A single bounded ACK line is flushed while the request stays open. Closing
	// this response or canceling the caller cancels the exact drain context.
	receipt, err := readServiceHostExitLine(limited)
	if err != nil || limited.N == 0 || receipt.Disposition != "accepted" || receipt.Challenge != envelope.Challenge {
		return refuse("envelope", nil)
	}

	if !stopACK() || serviceHostExitAdmissionCause(ackCtx, ctx, time.Now()) != nil {
		return refuse("envelope", serviceHostExitAdmissionCause(ackCtx, ctx, time.Now()))
	}
	completionCtx, completionCancel := context.WithTimeout(requestCtx, serviceHostExitPhaseBudget(requestCtx, ServiceShutdownTimeout))
	stopCompletion := context.AfterFunc(completionCtx, cancel)
	session.ctx = completionCtx
	session.cancel = func() { stopCompletion(); completionCancel(); cancel() }
	return session, nil
}

func (c *Cell) hostExitRequest(ctx context.Context, envelope serviceHostExitEnvelope) error {
	session, err := c.startHostExitRequest(ctx, envelope)
	if err != nil {
		return err
	}
	// ACK-only callers retain this bounded stream through completion. Their
	// context still cancels it; no background observer can outlive the tree bound.
	go func() { defer session.cancel(); session.finish() }()
	return nil
}

func (c *Cell) newHostExitEnvelope() (serviceHostExitEnvelope, serviceLeaseRecord, error) {
	d, lease, err := c.serviceHostExitIdentity()
	if err != nil {
		return serviceHostExitEnvelope{}, lease, err
	}
	var challenge [16]byte
	if _, err := rand.Read(challenge[:]); err != nil {
		return serviceHostExitEnvelope{}, lease, hostExitRefused("envelope")
	}
	return serviceHostExitEnvelope{InstallationID: d.InstallationID, Generation: d.Payload.Generation, HostPID: lease.PID, BootID: lease.BootID, Challenge: strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(challenge[:]))}, lease, nil
}

func (c *Cell) controlExitHost(ctx context.Context) (ServiceStatus, error) {
	envelope, lease, err := c.newHostExitEnvelope()
	if err != nil {
		return ServiceStatus{}, err
	}
	observer, err := retainServiceExitHost(lease)
	if err != nil {
		return ServiceStatus{}, err
	}
	defer observer.close()
	if err := c.hostExitRequest(ctx, envelope); err != nil {
		return ServiceStatus{}, err
	}
	return ServiceStatus{Generation: envelope.Generation, HostExit: "accepted"}, nil
}

// ExitServiceHost requests an authenticated drain and then observes the exact
// host's exit. It preserves desired state so a same-boot restart can resume.
func (c *Cell) ExitServiceHost(ctx context.Context) (ServiceStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, serviceHostExitPhaseBudget(ctx, ServiceHostExitBudget(c.cfg.Timeout)))
	defer cancel()
	envelope, lease, err := c.newHostExitEnvelope()
	if err != nil {
		return ServiceStatus{}, hostExitFailure("identity_tree", false, err)
	}
	observer, err := retainServiceExitHost(lease)
	if err != nil {
		return ServiceStatus{}, hostExitFailure("retain", false, err)
	}
	defer observer.close()
	session, err := c.startHostExitRequest(ctx, envelope)
	if err != nil {
		return ServiceStatus{}, hostExitFailure("endpoint", false, errors.Join(err, ctx.Err()))
	}
	status := ServiceStatus{Generation: envelope.Generation, HostExit: "accepted"}
	streamDone := make(chan struct{})
	go func() { defer close(streamDone); session.finish() }()
	defer func() { session.cancel(); <-streamDone }()
	if err := c.waitServiceHostExit(session.ctx, lease, observer); err != nil {
		return status, hostExitFailure("observe", true, errors.Join(err, session.ctx.Err()))
	}
	<-streamDone
	status.HostExit = "exited"
	return status, hostExitFailure("terminal", true, errors.Join(session.err, session.ctx.Err()))
}

// Probe the stable lock without creating, rewriting, or unlinking its identity.
func (c *Cell) serviceHostLeaseHeld() (bool, error) {
	file, err := openLockFile(c.path(".service-host.lock"), false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, hostExitRefused("envelope")
	}
	defer func() { _ = file.Close() }()
	if err := lockFile(file); errors.Is(err, ErrLocked) {
		return true, nil
	} else if err != nil {
		return false, hostExitRefused("envelope")
	}
	if unlockFile(file) != nil {
		return false, hostExitRefused("envelope")
	}
	return false, nil
}

// Called before Remove obtains the operation lock: the host's child drain needs
// that lock. The stopped fence is durable before the exit request is sent.
func (c *Cell) exitServiceHostForRemoval(ctx context.Context) error {
	if _, err := c.ServiceDescriptor(); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return hostExitRefused("envelope")
	}
	held, err := c.serviceHostLeaseHeld()
	if err != nil {
		return err
	}
	if !held {
		return c.SetServiceDesiredState(ctx, "stopped")
	}
	// The live host owns its endpoint revision. Persist intent through its
	// authenticated stop command so Windows endpoint discovery remains bound
	// to that owner's descriptor publication, then request host exit.
	if _, err := c.Control(ctx, "stop"); err != nil {
		return hostExitRefused("drain")
	}
	envelope, lease, err := c.newHostExitEnvelope()
	if err != nil {
		return err
	}
	observer, err := retainServiceExitHost(lease)
	if err != nil {
		return err
	}
	defer observer.close()
	session, err := c.startHostExitRequest(ctx, envelope)
	if err != nil {
		return err
	}
	streamDone := make(chan struct{})
	go func() { defer close(streamDone); session.finish() }()
	defer func() { session.cancel(); <-streamDone }()
	if err := c.waitServiceHostExit(session.ctx, lease, observer); err != nil {
		return err
	}
	<-streamDone
	return session.err
}

type serviceExitHostObserver interface {
	exited() (bool, error)
	close()
}

func (c *Cell) waitServiceHostExit(ctx context.Context, lease serviceLeaseRecord, observer serviceExitHostObserver) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return hostExitFailure("observe", true, errors.Join(hostExitRefused("drain"), ctx.Err()))
		}
		var current serviceLeaseRecord
		if err := servicePrivateRecord(c.path("service", "lease.json")); err != nil {
			return hostExitFailure("lease_validate", true, errors.Join(hostExitRefused("envelope"), err))
		}
		if err := readJSON(c.path("service", "lease.json"), &current); err != nil || current != lease {
			return hostExitFailure("lease_validate", true, errors.Join(hostExitRefused("envelope"), err))
		}
		held, err := c.serviceHostLeaseHeld()
		if err != nil {
			return hostExitFailure("lease_lock", true, err)
		}
		exited, err := observer.exited()
		if err != nil {
			return err
		}
		if !held && exited {
			for _, component := range []string{"worker", "server", "postgres"} {
				var record ProcessRecord
				if err := readJSON(c.path("run", component+".json"), &record); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil {
					return hostExitFailure("observe", true, errors.Join(hostExitRefused("drain"), ctx.Err()))
				}
				if record.Root != c.cfg.Root || record.Component != component || record.PID <= 1 || record.StartTime == "" {
					return hostExitFailure("observe", true, errors.Join(hostExitRefused("drain"), ctx.Err()))
				}
				if _, alive, err := processIdentity(record.PID); err != nil || alive {
					return hostExitFailure("observe", true, errors.Join(hostExitRefused("drain"), ctx.Err()))
				}
			}
			return nil
		}
		if held {
			// A refused drain leaves the host responsive, allowing observation to fail
			// promptly instead of interpreting an accepted ACK as an exit receipt.
			observeCtx, observeCancel := context.WithTimeout(ctx, 100*time.Millisecond)
			status, statusErr := c.Control(observeCtx, "observe")
			observeCancel()
			if statusErr == nil && (status.HostExit == "refused" || status.State == "faulted") {
				return hostExitFailure("observe", true, errors.Join(hostExitRefused("drain"), ctx.Err()))
			}
		}
		select {
		case <-ctx.Done():
			return hostExitFailure("observe", true, errors.Join(hostExitRefused("drain"), ctx.Err()))
		case <-ticker.C:
		}
	}
}
