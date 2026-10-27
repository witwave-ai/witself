package localcell

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

func (c *Cell) readServiceControlSecret() ([]byte, error) {
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
	if err := c.validateServiceSocket(); err != nil {
		return ServiceStatus{}, err
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.path("service", "control.sock"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localcell/"+action, nil)
	if err != nil {
		return ServiceStatus{}, ErrServiceControlRefused
	}
	request.Header.Set("Authorization", "Bearer "+string(secret))
	response, err := client.Do(request)
	if err != nil {
		return ServiceStatus{}, errors.New("localcell: service_control_unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized {
		return ServiceStatus{}, ErrServiceControlRefused
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

func validServiceAction(action string) bool {
	switch action {
	case "status", "health", "start", "stop", "repair":
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
	server := &http.Server{ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096}
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
		command := serviceCommand{ctx: r.Context(), action: action, reply: make(chan serviceReply, 1)}
		select {
		case commands <- command:
		case <-r.Context().Done():
			return
		case <-closed:
			return
		}
		select {
		case reply := <-command.reply:
			if reply.err != nil {
				http.Error(w, "service_control_action_failed", http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(reply.status)
		case <-r.Context().Done():
		case <-closed:
		}
	})
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	return func() { close(closed); _ = server.Close(); <-done; _ = c.removeServiceSocket() }, nil
}
