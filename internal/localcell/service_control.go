package localcell

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

type serviceControlCommandKey struct{}
type serviceControlHealthBoundKey struct{}
type serviceControlHostContextKey struct{}

// The caller abandoned this command. Preserve its error identity and site for
// control responses, while keeping it out of the host's failure diagnostics.
type serviceControlClientCancellation struct{ cause error }

func (e *serviceControlClientCancellation) Error() string { return e.cause.Error() }
func (e *serviceControlClientCancellation) Unwrap() error { return e.cause }

var errServiceControlDeferred = errors.New("localcell: service_control_deferred")

func serviceControlCancellation(ctx context.Context) error {
	if control, _ := ctx.Value(serviceControlCommandKey{}).(bool); control && ctx.Err() != nil {
		err := serviceFailureAt(ctx.Err(), "control_cancelled")
		host, _ := ctx.Value(serviceControlHostContextKey{}).(context.Context)
		if host == nil || host.Err() == nil {
			return &serviceControlClientCancellation{cause: err}
		}
		return err
	}
	return nil
}

func serviceControlHealthBound(ctx context.Context, hostBound time.Duration) time.Duration {
	if callerBound, ok := ctx.Value(serviceControlHealthBoundKey{}).(time.Duration); ok && callerBound > 0 {
		return min(hostBound, callerBound)
	}
	return hostBound
}

// Reserve proportional room for a queued tick, the health decision and response
// delivery. Saturation prevents a caller's large duration from wrapping negative.
func serviceControlBudget(bound time.Duration, shares int) time.Duration {
	const maximum = time.Duration(1<<63 - 1)
	if bound > maximum/time.Duration(shares) {
		return maximum
	}
	return bound * time.Duration(shares)
}

func (c *Cell) readServiceControlSecret() (secretBytes []byte, result error) {
	defer func() { result = serviceFailureAt(result, "control_secret") }()
	if err := c.validateRoot(false); err != nil {
		return nil, ErrServiceControlRefused
	}
	secret, err := c.serviceControlSecret()
	if err != nil {
		return nil, ErrServiceControlRefused
	}
	return []byte(secret), nil
}

// Control reads the private installation token directly into the request. The
// token never appears in arguments, responses or diagnostic errors.
func (c *Cell) Control(ctx context.Context, action string) (ServiceStatus, error) {
	if os.Getenv("WITSELF_RUNTIME") != "local" {
		return ServiceStatus{}, ErrServiceControlRefused
	}
	if err := serviceControlCapability(); err != nil {
		return ServiceStatus{}, err
	}
	secret, err := c.readServiceControlSecret()
	if err != nil {
		return ServiceStatus{}, err
	}
	if !validServiceAction(action) {
		return ServiceStatus{}, ErrServiceControlRefused
	}
	if err := c.validateServiceControlEndpoint(); err != nil {
		return ServiceStatus{}, err
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := c.dialServiceControl(ctx)
		if err != nil {
			return nil, &serviceControlDialError{err: err}
		}
		return conn, nil
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: serviceControlBudget(c.cfg.Timeout, 4), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localcell/"+action, nil)
	if err != nil {
		return ServiceStatus{}, ErrServiceControlRefused
	}
	request.Header.Set("Authorization", "Bearer "+string(secret))
	request.Header.Set("X-Localcell-Health-Bound", c.cfg.Timeout.String())
	response, err := client.Do(request)
	if err != nil {
		return ServiceStatus{}, serviceControlTransportError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized {
		return ServiceStatus{}, ErrServiceControlRefused
	}
	if response.StatusCode == http.StatusAccepted {
		return ServiceStatus{}, errServiceControlDeferred
	}
	if action == "health" && response.StatusCode == http.StatusServiceUnavailable {
		return ServiceStatus{}, errServiceUnhealthy
	}
	if response.StatusCode != http.StatusOK {
		return ServiceStatus{}, errors.New("localcell: service_control_action_failed")
	}
	var status ServiceStatus
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&status) != nil || decoder.Decode(new(any)) != io.EOF {
		return ServiceStatus{}, ErrServiceControlRefused
	}
	return status, nil
}

// Keep the dial boundary in the error chain without exposing its address or the
// platform error. Timeout and refusal causes retain precedence over this marker.
type serviceControlDialError struct{ err error }

func (e *serviceControlDialError) Error() string { return "service_control_dial_failed" }
func (e *serviceControlDialError) Unwrap() error { return e.err }

// net/http wraps transport failures in URL errors. Only this closed vocabulary
// crosses Control's public boundary; in particular pipe:io remains the original
// typed refusal. Do not append raw errors, URLs, endpoint names or timings.
func serviceControlTransportError(err error) error {
	var refusal *serviceControlRefusal
	if errors.As(err, &refusal) {
		var located *serviceFailure
		if errors.As(err, &located) {
			return serviceFailureAt(refusal, located.site)
		}
		return refusal
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		serviceControlTransportTimeout(err) {
		return errors.New("localcell: service_control_unavailable: transport:deadline")
	}
	var dial *serviceControlDialError
	if errors.As(err, &dial) {
		return errors.New("localcell: service_control_unavailable: transport:dial")
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("localcell: service_control_unavailable: transport:eof")
	}
	return errors.New("localcell: service_control_unavailable: transport:response")
}

func serviceControlTransportTimeout(err error) bool {
	if err == nil {
		return false
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		return true
	}
	// A URL error can itself implement net.Error yet report false when an
	// intermediate wrapper does not implement Timeout. Inspect every cause.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if serviceControlTransportTimeout(cause) {
				return true
			}
		}
		return false
	}
	return serviceControlTransportTimeout(errors.Unwrap(err))
}

func validServiceAction(action string) bool {
	switch action {
	case "observe", "status", "health", "start", "stop", "repair":
		return true
	}
	return false
}

func (c *Cell) serveServiceControl(commands chan<- serviceCommand) (func(), error) {
	secret, err := c.readServiceControlSecret()
	if err != nil {
		return nil, err
	}
	listener, err := c.listenServiceControl()
	if err != nil {
		return nil, err
	}
	closed := make(chan struct{})
	server := &http.Server{ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: serviceControlBudget(c.cfg.Timeout, 4), IdleTimeout: time.Second, MaxHeaderBytes: 4096}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), append([]byte("Bearer "), secret...)) != 1 {
			http.Error(w, "service_control_refused", http.StatusUnauthorized)
			return
		}
		action := r.URL.Path[1:]
		if r.Method != http.MethodPost || r.URL.RawQuery != "" || !validServiceAction(action) {
			http.Error(w, "service_control_refused", http.StatusBadRequest)
			return
		}
		bound := c.cfg.Timeout
		if callerBound, err := time.ParseDuration(r.Header.Get("X-Localcell-Health-Bound")); err == nil && callerBound > 0 {
			bound = min(bound, callerBound)
		}
		// This bounds waiting for a queue/decision, not the host's health
		// verdict. A tighter caller bound also cancels the accepted command.
		commandCtx, cancel := context.WithTimeout(r.Context(), serviceControlBudget(bound, 3))
		defer cancel()
		commandCtx = context.WithValue(commandCtx, serviceControlHealthBoundKey{}, bound)
		refuse := func(err error) {
			if errors.Is(err, errServiceControlDeferred) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				http.Error(w, "service_control_deferred", http.StatusAccepted)
			} else if action == "health" && errors.Is(err, errServiceUnhealthy) {
				http.Error(w, "service_unhealthy", http.StatusServiceUnavailable)
			} else {
				http.Error(w, "service_control_action_failed", http.StatusConflict)
			}
		}
		command := serviceCommand{ctx: commandCtx, action: action, reply: make(chan serviceReply, 1)}
		select {
		case commands <- command:
		case <-commandCtx.Done():
			refuse(commandCtx.Err())
			return
		case <-closed:
			return
		}
		select {
		case reply := <-command.reply:
			if reply.err != nil {
				refuse(reply.err)
				return
			}
			writeServiceControlResponse(w, action, reply.status, os.Stderr)
		case <-commandCtx.Done():
			refuse(commandCtx.Err())
		case <-closed:
		}
	})
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	return func() {
		close(closed)
		_ = server.Close()
		<-done
		WriteServiceHostFailure(os.Stderr, c.removeServiceControlEndpoint(), "endpoint_remove")
	}, nil
}

// This closed vocabulary is shared by accept and stop-response failures.
// Never forward error text, process identities or paths to stderr.
func writeServiceControlDiagnostic(output io.Writer, line string) {
	switch line {
	case "service_control accept=rejected step=peer:pid",
		"service_control accept=rejected step=peer:identity",
		"service_control accept=rejected step=peer:sid",
		"service_control accept=rejected step=duplicate",
		"service_control accept=rejected step=conn",
		"service_control action=stop response=unflushed":
		_, _ = fmt.Fprintln(output, line)
	}
}

func serviceControlAcceptRejected(output io.Writer, step string) {
	writeServiceControlDiagnostic(output, "service_control accept=rejected step="+step)
}

func writeServiceControlResponse(w http.ResponseWriter, action string, status ServiceStatus, diagnostic io.Writer) {
	body, err := json.Marshal(status)
	if err != nil {
		return
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	// A fixed length makes the flushed bytes a complete HTTP response; chunked
	// encoding would defer its terminal chunk until after the handler returns.
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if written, err := w.Write(body); err != nil || written != len(body) {
		if action == "stop" {
			writeServiceControlDiagnostic(diagnostic, "service_control action=stop response=unflushed")
		}
		return
	}
	if action == "stop" && http.NewResponseController(w).Flush() != nil {
		writeServiceControlDiagnostic(diagnostic, "service_control action=stop response=unflushed")
	}
}
