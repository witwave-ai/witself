//go:build darwin || linux

package localcell

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestServiceControlTransportRoundTrip(t *testing.T) {
	for _, kind := range []string{"eof", "deadline", "dial"} {
		t.Run(kind, func(t *testing.T) {
			c := shortServiceTestCell(t)
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: c.path("service", "control.sock"), Net: "unix"})
			if err != nil {
				t.Fatal("cannot create transport fixture socket")
			}
			listener.SetUnlinkOnClose(false)
			t.Cleanup(func() { _ = listener.Close() })
			if os.Chmod(c.path("service", "control.sock"), 0o600) != nil {
				t.Fatal("cannot protect transport fixture socket")
			}
			if kind == "dial" {
				// Leave a private socket inode, so endpoint validation succeeds and
				// the real dial fails inside the HTTP transport.
				_ = listener.Close()
			} else {
				done := make(chan struct{})
				release := make(chan struct{})
				t.Cleanup(func() { close(release); _ = listener.Close(); <-done })
				go func() {
					defer close(done)
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					defer func() { _ = conn.Close() }()
					_ = conn.SetReadDeadline(time.Now().Add(time.Second))
					request, err := http.ReadRequest(bufio.NewReader(conn))
					if err != nil {
						return
					}
					_ = request.Body.Close()
					if kind == "deadline" {
						<-release
					}
				}()
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, err = c.Control(ctx, "health")
			if err == nil || err.Error() != "localcell: service_control_unavailable: transport:"+kind {
				t.Fatal("Unix control round trip lost its transport classification")
			}
		})
	}
}
