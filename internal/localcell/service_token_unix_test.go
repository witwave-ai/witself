//go:build darwin || linux

package localcell

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServiceSupervisorControlOutputsExcludeControlToken(t *testing.T) {
	c := shortServiceTestCell(t)
	token := installServiceFixtureControlToken(t, c)
	descriptor, err := c.ServiceDescriptor()
	if err != nil {
		t.Fatal("cannot read synthetic service descriptor")
	}
	hooks, _, _, _, _ := supervisorTestHooks()
	supervisor := newSupervisor(c, SupervisorPolicy{hooks: &hooks})
	if err := supervisor.load(descriptor); err != nil {
		t.Fatal("cannot load synthetic supervisor")
	}
	commands := make(chan serviceCommand)
	closeControl, err := c.serveServiceControl(commands)
	if err != nil {
		t.Fatal("cannot start synthetic control endpoint")
	}
	defer closeControl()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case command := <-commands:
				status, err := supervisor.command(command.ctx, command.action)
				command.reply <- serviceReply{status: status, err: err}
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); <-done }()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.path("service", "control.sock"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	for _, action := range []string{"status", "stop"} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://localcell/"+action, nil)
		if err != nil {
			t.Fatal("cannot construct synthetic control request")
		}
		request.Header.Set("Authorization", "Bearer "+string(token))
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("synthetic authenticated control request failed")
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || len(body) > 4096 {
			t.Fatal("synthetic control endpoint did not return bounded successful output")
		}
		assertServiceOutputExcludesToken(t, "control "+action+" response", body, token)
		var status ServiceStatus
		if json.Unmarshal(body, &status) != nil {
			t.Fatal("control endpoint did not serialize supervisor status")
		}
		if action == "status" && (!status.Healthy || status.State != "running") {
			t.Fatal("control status did not observe the running supervisor")
		}
		if action == "stop" && (status.Healthy || status.State != "stopped") {
			t.Fatal("control stop receipt did not observe the stopped supervisor")
		}
	}
}
