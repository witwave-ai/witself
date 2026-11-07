//go:build darwin || linux

package localcell

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServiceShutdownControlResponseDeadline(t *testing.T) {
	for _, action := range []string{"stop", "repair"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("WITSELF_HOME", t.TempDir())
			t.Setenv("DSH_HOME", t.TempDir())
			started := time.Now()
			c := shortServiceTestCell(t)
			descriptor, err := c.ServiceDescriptor()
			if err != nil {
				t.Fatal("descriptor unavailable")
			}
			lease, err := c.acquireServiceLease(descriptor)
			if err != nil {
				t.Fatal("lease unavailable")
			}
			defer func() { _ = lease.Close() }()
			// Use this fixture's observed setup cost as its timing scale. The
			// floor accommodates a clock tick; no latency bound is asserted.
			c.cfg.Timeout = max(time.Since(started), time.Millisecond)
			oldWriteLimit := serviceControlBudget(c.cfg.Timeout, 4)
			drain := 2 * oldWriteLimit
			commands := make(chan serviceCommand)
			closeControl, err := c.serveServiceControlForPlatform(commands, "windows")
			if err != nil {
				t.Fatal("control listener unavailable")
			}
			defer closeControl()
			responderDone := make(chan struct{})
			go func() {
				defer close(responderDone)
				select {
				case command := <-commands:
					timer := time.NewTimer(drain)
					defer timer.Stop()
					select {
					case <-timer.C:
						command.reply <- serviceReply{status: ServiceStatus{State: "stopped"}}
					case <-command.ctx.Done():
					}
				case <-t.Context().Done():
				}
			}()
			secret, err := c.readServiceControlSecret()
			if err != nil {
				t.Fatal("control material unavailable")
			}
			transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dialServiceControl(ctx) }}
			defer transport.CloseIdleConnections()
			// This behavioral watchdog scales with the measured fixture cost.
			client := &http.Client{Transport: transport, Timeout: 32 * drain}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://localcell/"+action, nil)
			if err != nil {
				t.Fatal("cannot prepare control request")
			}
			request.Header.Set("Authorization", "Bearer "+string(secret))
			response, err := client.Do(request)
			if err != nil {
				t.Fatal("valid Windows drain lost its response at the old write deadline")
			}
			defer func() { _ = response.Body.Close() }()
			var status ServiceStatus
			if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&status) != nil || status.State != "stopped" {
				t.Fatal("valid Windows drain did not deliver the completed response")
			}
			<-responderDone
		})
	}
}
