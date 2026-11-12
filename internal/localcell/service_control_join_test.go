package localcell

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

// A pipe exercises net/http's real accept and connection goroutines on every
// platform, while the response writer supplies a deterministic post-reply gate.
type serviceControlJoinListener struct {
	conn     net.Conn
	accepted bool
	closed   chan struct{}
	once     sync.Once
}

func (l *serviceControlJoinListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *serviceControlJoinListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *serviceControlJoinListener) Addr() net.Addr { return l.conn.LocalAddr() }

type serviceControlJoinConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *serviceControlJoinConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

func serviceControlJoinCapture(t *testing.T) func() string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "control-stderr-")
	if err != nil {
		t.Fatal("cannot create control shutdown diagnostic capture")
	}
	previous := os.Stderr
	os.Stderr = file
	// Registered before any worker cleanup: no emitter may outlive this capture.
	t.Cleanup(func() { os.Stderr = previous; _ = file.Close() })
	return func() string {
		data, err := os.ReadFile(file.Name())
		if err != nil {
			t.Fatal("cannot read control shutdown diagnostic capture")
		}
		return string(data)
	}
}

type serviceControlJoinFixture struct {
	budget  time.Duration
	bound   time.Duration
	conn    *serviceControlJoinConn
	writer  *serviceControlFlushWriter
	finish  <-chan struct{}
	stop    func()
	release func()
	force   func()
}

func newServiceControlJoinFixture(t *testing.T, failFlush bool) *serviceControlJoinFixture {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	conn := &serviceControlJoinConn{Conn: serverConn, closed: make(chan struct{})}
	listener := &serviceControlJoinListener{conn: conn, closed: make(chan struct{})}
	writer := &serviceControlFlushWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	if failFlush {
		writer.flushError = errors.New("fixture response flush failed")
	}
	// A one-second join leaves the post-reply release ample room on a loaded CI
	// runner; only the stuck-handler test waits the whole budget.
	budget := serviceControlBudget(250*time.Millisecond, 4)
	bound := serviceControlBudget(budget, 8)
	var releaseOnce, stopOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(writer.release) }) }
	finish := make(chan struct{})
	started := make(chan struct{})
	replies := make(chan ServiceStatus, 1)
	replies <- ServiceStatus{DesiredState: "stopped"}
	server := &http.Server{ReadHeaderTimeout: bound, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		defer close(finish)
		close(started)
		reply := <-replies
		writeServiceControlResponse(writer, "stop", reply, os.Stderr)
	})}
	stop := startServiceControlServer(server, listener, make(chan struct{}), budget)
	f := &serviceControlJoinFixture{budget: budget, bound: bound, conn: conn, writer: writer, finish: finish, release: release}
	f.stop = func() { stopOnce.Do(stop) }
	f.force = func() {
		release()
		_ = clientConn.Close()
		_ = listener.Close()
		_ = server.Close()
	}
	t.Cleanup(func() {
		f.force()
		f.stop()
		select {
		case <-started:
			select {
			case <-finish:
			case <-time.After(bound):
				t.Error("response handler outlived fixture cleanup")
			}
		default:
		}
	})
	request := StartJoinedTestRunForTest(t, f.force, bound, func() error {
		_, err := io.WriteString(clientConn, "POST /stop HTTP/1.1\r\nHost: localcell\r\nContent-Length: 0\r\n\r\n")
		return err
	})
	select {
	case err := <-request:
		if err != nil {
			t.Fatal("cannot send fixture control request")
		}
	case <-time.After(bound):
		t.Fatal("fixture control request did not reach the server")
	}
	select {
	case <-writer.entered:
	case <-time.After(bound):
		t.Fatal("response handler did not reach its post-reply flush gate")
	}
	return f
}

func (f *serviceControlJoinFixture) stopAsync(t *testing.T) <-chan error {
	t.Helper()
	return StartJoinedTestRunForTest(t, f.force, f.bound, func() error { f.stop(); return nil })
}

func TestServiceControlStopJoinsPostReplyHandler(t *testing.T) {
	output := serviceControlJoinCapture(t)
	f := newServiceControlJoinFixture(t, true)
	stopped := f.stopAsync(t)
	select {
	case <-f.conn.closed:
	case <-time.After(f.bound):
		t.Fatal("control shutdown did not close its active connection")
	}
	select {
	case <-stopped:
		t.Fatal("control stop returned before the post-reply writer finished")
	case <-time.After(f.budget / 4):
	}
	if output() != "" {
		t.Fatal("blocked response emitted a diagnostic before its writer finished")
	}
	f.release()
	select {
	case <-stopped:
	case <-time.After(f.bound):
		t.Fatal("control stop did not join the released response handler")
	}
	atReturn := output()
	if atReturn != "service_control action=stop response=unflushed\n" {
		t.Fatal("control stop returned before the response diagnostic completed")
	}
	select {
	case <-f.finish:
	default:
		t.Fatal("control stop returned before the response handler exited")
	}
	if output() != atReturn {
		t.Fatal("response diagnostic reached stderr after control stop returned")
	}
}

func TestServiceControlStopBoundsStuckHandler(t *testing.T) {
	output := serviceControlJoinCapture(t)
	f := newServiceControlJoinFixture(t, false)
	stopped := f.stopAsync(t)
	select {
	case <-stopped:
	case <-time.After(f.bound):
		// Release the synthetic stuck handler before failing. An unbounded
		// join mutant must fail this watchdog, not hang the package timeout.
		f.force()
		select {
		case <-stopped:
		case <-time.After(f.bound):
			t.Error("control stop did not recover after releasing its stuck fixture")
		}
		t.Fatal("control stop exceeded its bounded handler join")
	}
	select {
	case <-f.finish:
		t.Fatal("stuck-handler fixture finished before shutdown exhausted its bound")
	default:
	}
	if output() != "service_control stop=handlers_unjoined\n" {
		t.Fatal("stuck handler did not emit exactly one fixed join diagnostic")
	}
	f.release()
	select {
	case <-f.finish:
	case <-time.After(f.bound):
		t.Fatal("released stuck handler did not finish")
	}
	if output() != "service_control stop=handlers_unjoined\n" {
		t.Fatal("stuck-handler shutdown emitted an additional diagnostic")
	}
}

func TestServiceControlStopClosesBeforeJoin(t *testing.T) {
	output := serviceControlJoinCapture(t)
	f := newServiceControlJoinFixture(t, false)
	StartJoinedTestRunForTest(t, f.force, f.bound, func() error {
		<-f.conn.closed
		f.release()
		return nil
	})
	stopped := f.stopAsync(t)
	select {
	case <-stopped:
	case <-time.After(f.bound):
		t.Fatal("control stop waited for its handler before closing the connection")
	}
	if output() != "" {
		t.Fatal("control stop exhausted its join budget before closing the connection")
	}
	select {
	case <-f.finish:
	default:
		t.Fatal("control stop failed to join the handler released by connection close")
	}
}

func TestServiceControlStopWithoutRequests(t *testing.T) {
	budget := serviceControlBudget(250*time.Millisecond, 4)
	for _, tc := range []struct {
		name   string
		budget time.Duration
	}{{"normal_budget", budget}, {"already_expired_budget", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			output := serviceControlJoinCapture(t)
			serverConn, peer := net.Pipe()
			defer func() { _ = peer.Close(); _ = serverConn.Close() }()
			listener := &serviceControlJoinListener{conn: serverConn, accepted: true, closed: make(chan struct{})}
			server := &http.Server{ReadHeaderTimeout: time.Second}
			closed := make(chan struct{})
			stop := startServiceControlServer(server, listener, closed, tc.budget)
			stopped := StartJoinedTestRunForTest(t, func() { _ = listener.Close(); _ = server.Close() }, serviceControlBudget(budget, 8), func() error {
				stop()
				return nil
			})
			select {
			case <-stopped:
			case <-time.After(serviceControlBudget(budget, 8)):
				t.Fatal("empty control server did not stop")
			}
			select {
			case <-closed:
			default:
				t.Fatal("empty control server did not notify command handlers of shutdown")
			}
			if output() != "" {
				t.Fatal("empty control server changed its silent stop behavior")
			}
		})
	}
}
