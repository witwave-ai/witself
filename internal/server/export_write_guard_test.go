package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	exportWriteTestIdle         = 200 * time.Millisecond
	exportWriteTestEvacuationID = "evac_write_deadline"
	exportWriteTestBackupID     = "bkp_write_deadline"
)

func exportWriteTestConfig(route string) Config {
	cfg := Config{ExportWriteStallTimeout: exportWriteTestIdle, CellName: "test-cell"}
	if route == "evacuation" {
		cfg.ProvisionToken = "witself_prv_write_deadline"
		cfg.ProvisionAccountExact = func(context.Context, string, string, string, string, string) (ProvisionedAccount, error) {
			return ProvisionedAccount{}, errors.New("unexpected provisioning call")
		}
	} else {
		cfg.BackupToken = "witself_bkp_write_deadline"
	}
	return cfg
}

func exportWriteTestStream(cfg *Config, route string, stream func(context.Context, io.Writer, func() error) error) {
	if route == "evacuation" {
		cfg.StreamAccountExport = func(ctx context.Context, _, _ string, w io.Writer, flush func() error) error {
			return stream(ctx, w, flush)
		}
	} else {
		cfg.StreamAccountBackup = func(ctx context.Context, _, _, _ string, w io.Writer, flush func() error) error {
			return stream(ctx, w, flush)
		}
	}
}

func exportWriteTestRequest(url, route string) *http.Request {
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		panic(err)
	}
	if route == "evacuation" {
		req.Header.Set("Authorization", "Bearer witself_prv_write_deadline")
		req.Header.Set(AccountEvacuationIDHeader, exportWriteTestEvacuationID)
	} else {
		req.Header.Set("Authorization", "Bearer witself_bkp_write_deadline")
		req.Header.Set(AccountBackupIDHeader, exportWriteTestBackupID)
	}
	return req
}

func exportWriteTestURL(srv *httptest.Server, route, accountID string) string {
	return srv.URL + "/v1/accounts/" + accountID + ":export-" + route
}

// The production instrumentation must participate: its ResponseWriter has to
// preserve ResponseController's route to the underlying connection.
func exportWriteTestServer(t *testing.T, cfg Config, socketBuffer int) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	metrics := newRuntimeMetrics()
	h := metrics.instrument(apiMux(metrics.instrumentConfig(cfg)))
	return exportWriteTestHandlerServer(t, h, socketBuffer)
}

// A zero socketBuffer keeps the platform defaults. Only stalled-reader tests
// shrink buffers to reach backpressure without depending on their exact capacity.
func exportWriteTestHandlerServer(t *testing.T, h http.Handler, socketBuffer int) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	returned := make(chan struct{}, 16)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { returned <- struct{}{} }()
		h.ServeHTTP(w, r)
	}))
	srv.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateNew && socketBuffer > 0 {
			if err := conn.(*net.TCPConn).SetWriteBuffer(socketBuffer); err != nil {
				t.Errorf("clamp server write buffer: %v", err)
			}
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, returned
}

// Read only through the header terminator; unlike a buffered read directly from
// the socket, this cannot consume archive body bytes before the test asks for them.
func exportWriteRawClient(t *testing.T, srv *httptest.Server, route, accountID string, d time.Duration, socketBuffer int) (*net.TCPConn, *http.Response) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), 20*d)
	if err != nil {
		t.Fatal(err)
	}
	tcp := conn.(*net.TCPConn)
	t.Cleanup(func() { _ = tcp.Close() })
	if socketBuffer > 0 {
		if err := tcp.SetReadBuffer(socketBuffer); err != nil {
			t.Fatal(err)
		}
	}
	if err := tcp.SetDeadline(time.Now().Add(20 * d)); err != nil {
		t.Fatal(err)
	}
	req := exportWriteTestRequest(exportWriteTestURL(srv, route, accountID), route)
	if _, err := fmt.Fprintf(tcp, "POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: %s\r\n%s: %s\r\nContent-Length: 0\r\n\r\n", req.URL.RequestURI(), req.URL.Host, req.Header.Get("Authorization"), map[string]string{"evacuation": AccountEvacuationIDHeader, "backup": AccountBackupIDHeader}[route], map[string]string{"evacuation": exportWriteTestEvacuationID, "backup": exportWriteTestBackupID}[route]); err != nil {
		t.Fatal(err)
	}
	var headers strings.Builder
	var one [1]byte
	for !strings.HasSuffix(headers.String(), "\r\n\r\n") {
		if headers.Len() > 32<<10 {
			t.Fatal("response headers exceed test bound")
		}
		if _, err := io.ReadFull(tcp, one[:]); err != nil {
			t.Fatalf("read response headers: %v", err)
		}
		headers.WriteByte(one[0])
	}
	resp, err := http.ReadResponse(bufio.NewReader(io.MultiReader(strings.NewReader(headers.String()), tcp)), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if err := tcp.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	return tcp, resp
}

type exportWriteTestFailure struct {
	accountID string
	err       error
}

func exportWriteTestReports(cfg *Config) chan exportWriteTestFailure {
	reports := make(chan exportWriteTestFailure, 8)
	cfg.ReportAccountExportFailure = func(_ context.Context, accountID string, err error) {
		reports <- exportWriteTestFailure{accountID: accountID, err: err}
	}
	return reports
}

func exportWriteAwait[T any](t *testing.T, ch <-chan T, within time.Duration, what string) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(within):
		t.Fatalf("timed out waiting for %s after %v", what, within)
		var zero T
		return zero
	}
}

func exportWriteNoReport(t *testing.T, reports <-chan exportWriteTestFailure, d time.Duration) {
	t.Helper()
	select {
	case report := <-reports:
		t.Fatalf("unexpected failure report for %s: %v", report.accountID, report.err)
	case <-time.After(5 * d):
	}
}

type exportWriteTestCall struct {
	start, end time.Time
	size       int
	err        error
}

type exportWriteRecordingWriter struct {
	writer io.Writer
	calls  []exportWriteTestCall
}

func (w *exportWriteRecordingWriter) Write(p []byte) (int, error) {
	call := exportWriteTestCall{start: time.Now(), size: len(p)}
	n, err := w.writer.Write(p)
	call.end, call.err = time.Now(), err
	w.calls = append(w.calls, call)
	return n, err
}

func TestExportWriteDeadlineStalledClient(t *testing.T) {
	for _, route := range []string{"evacuation", "backup"} {
		t.Run(route, func(t *testing.T) {
			d := exportWriteTestIdle
			cfg := exportWriteTestConfig(route)
			reports := exportWriteTestReports(&cfg)
			type result struct {
				calls  []exportWriteTestCall
				ctxErr error
			}
			done := make(chan result, 1)
			exportWriteTestStream(&cfg, route, func(ctx context.Context, w io.Writer, _ func() error) error {
				recorded := &exportWriteRecordingWriter{writer: w}
				defer func() { done <- result{calls: recorded.calls, ctxErr: ctx.Err()} }()
				random := rand.New(rand.NewPCG(114, 1))
				block := make([]byte, 64<<10)
				for {
					for i := range block {
						block[i] = byte(random.Uint32())
					}
					if _, err := recorded.Write(block); err != nil {
						return err
					}
				}
			})
			srv, returned := exportWriteTestServer(t, cfg, 16<<10)
			_, _ = exportWriteRawClient(t, srv, route, "acc_write_deadline", d, 16<<10)
			got := exportWriteAwait(t, done, 20*d, "stalled write deadline")
			last := got.calls[len(got.calls)-1]
			elapsed := last.end.Sub(last.start)
			t.Logf("failing write: size=%d duration=%v", last.size, elapsed)
			if !errors.Is(last.err, os.ErrDeadlineExceeded) {
				t.Fatalf("write error = %v, want deadline exceeded", last.err)
			}
			if elapsed < d/2 || elapsed > 20*d {
				t.Fatalf("failing write duration = %v, want [%v, %v]", elapsed, d/2, 20*d)
			}
			if got.ctxErr == nil {
				t.Fatal("callback context was not canceled immediately after write failed")
			}
			report := exportWriteAwait(t, reports, 20*d, "failure report")
			if report.accountID != "acc_write_deadline" || !errors.Is(report.err, last.err) {
				t.Fatalf("report account=%s error=%v, want original write error", report.accountID, report.err)
			}
			exportWriteAwait(t, returned, 20*d, "handler return")
			exportWriteNoReport(t, reports, d)
		})
	}
}

var errExportWriteNoDeadline = errors.New("export write: no write deadline armed")

type exportWriteProgressOp struct {
	kind            string
	start, deadline time.Time
	err             error
}

// This connection-shaped response writer makes every write and flush wait for
// progress without relying on an operating system's socket-buffer capacity.
type exportWriteProgressConn struct {
	*httptest.ResponseRecorder
	hold     time.Duration
	mu       sync.Mutex
	deadline time.Time
	sets     int
	ops      []exportWriteProgressOp
}

func (c *exportWriteProgressConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = deadline
	c.sets++
	return nil
}

func (c *exportWriteProgressConn) take(kind string) error {
	c.mu.Lock()
	op := exportWriteProgressOp{kind: kind, start: time.Now(), deadline: c.deadline}
	c.mu.Unlock()
	time.Sleep(c.hold)
	// Judge the earliest possible completion, so a late wake-up cannot make a
	// freshly armed deadline fail. Earlier operations still consume real time.
	if op.deadline.IsZero() {
		op.err = errExportWriteNoDeadline
	} else if !op.start.Add(c.hold).Before(op.deadline) {
		op.err = os.ErrDeadlineExceeded
	}
	c.mu.Lock()
	c.ops = append(c.ops, op)
	c.mu.Unlock()
	return op.err
}

func (c *exportWriteProgressConn) Write(p []byte) (int, error) {
	if err := c.take("write"); err != nil {
		return 0, err
	}
	return c.ResponseRecorder.Write(p)
}

func (c *exportWriteProgressConn) FlushError() error {
	if err := c.take("flush"); err != nil {
		return err
	}
	c.Flush()
	return nil
}

func TestExportWriteProgressConnHonorsDeadline(t *testing.T) {
	d := exportWriteTestIdle
	conn := &exportWriteProgressConn{ResponseRecorder: httptest.NewRecorder(), hold: 3 * d / 5}
	block := []byte("progress")
	if n, err := conn.Write(block); n != 0 || !errors.Is(err, errExportWriteNoDeadline) {
		t.Fatalf("unarmed write: n=%d error=%v, want no write deadline armed", n, err)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
	if n, err := conn.Write(block); n != len(block) || err != nil {
		t.Fatalf("freshly armed write: n=%d error=%v", n, err)
	}
	if n, err := conn.Write(block); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write without re-arm: n=%d error=%v, want deadline exceeded", n, err)
	}
}

func TestExportWriteDeadlineProgressingReader(t *testing.T) {
	for _, route := range []string{"evacuation", "backup"} {
		t.Run(route, func(t *testing.T) {
			d := exportWriteTestIdle
			const groups = 4
			cfg := exportWriteTestConfig(route)
			reports := exportWriteTestReports(&cfg)
			block := bytes.Repeat([]byte("p"), 1<<10)
			var elapsed time.Duration
			var streamErr, ctxErr error
			exportWriteTestStream(&cfg, route, func(ctx context.Context, w io.Writer, flush func() error) (err error) {
				start := time.Now()
				defer func() { elapsed, streamErr, ctxErr = time.Since(start), err, ctx.Err() }()
				for range groups {
					for range 2 {
						if _, err = w.Write(block); err != nil {
							return err
						}
					}
					if err = flush(); err != nil {
						return err
					}
				}
				return nil
			})
			metrics := newRuntimeMetrics()
			h := metrics.instrument(apiMux(metrics.instrumentConfig(cfg)))
			conn := &exportWriteProgressConn{ResponseRecorder: httptest.NewRecorder(), hold: 3 * d / 5}
			req := exportWriteTestRequest("http://example.test/v1/accounts/acc_write_deadline:export-"+route, route)
			began := time.Now()
			h.ServeHTTP(conn, req)
			total := time.Since(began)
			t.Logf("progressing reader: operations=%d deadline sets=%d hold=%v callback=%v total=%v (%.2f*d)", len(conn.ops), conn.sets, conn.hold, elapsed, total, float64(total)/float64(d))
			for i, op := range conn.ops {
				if op.err != nil {
					t.Fatalf("op %d (%s) failed: %v (deadline-start=%v)", i, op.kind, op.err, op.deadline.Sub(op.start))
				}
			}
			if streamErr != nil || ctxErr != nil {
				t.Fatalf("callback error=%v context error=%v, want both nil", streamErr, ctxErr)
			}
			if len(conn.ops) != groups*3+1 || conn.sets < len(conn.ops) {
				t.Fatalf("operations=%d deadline sets=%d, want %d operations and at least as many sets", len(conn.ops), conn.sets, groups*3+1)
			}
			if last := conn.ops[len(conn.ops)-1]; last.kind != "flush" {
				t.Fatalf("last operation=%s, want finish flush", last.kind)
			}
			if total < 5*d || elapsed < 5*d {
				t.Fatalf("test proves nothing: callback=%v total=%v, want each >= %v", elapsed, total, 5*d)
			}
			if conn.Code != http.StatusOK || !bytes.Equal(conn.Body.Bytes(), bytes.Repeat(block, groups*2)) {
				t.Fatalf("status=%d body bytes=%d, want 200 and %d unchanged bytes", conn.Code, conn.Body.Len(), groups*2*len(block))
			}
			// Reporting is synchronous; ServeHTTP has returned, including finish.
			select {
			case report := <-reports:
				t.Fatalf("unexpected failure report for %s: %v", report.accountID, report.err)
			default:
			}
		})
	}
}

func TestExportWriteDeadlineRealTCPSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("real TCP export smoke test")
	}
	for _, route := range []string{"evacuation", "backup"} {
		t.Run(route, func(t *testing.T) {
			// Drain continuously with default socket buffers and ample scheduling
			// headroom. Backpressure belongs to the stalled-reader tests; deadline
			// renewal over a long stream is proved by ProgressingReader.
			d := 10 * exportWriteTestIdle
			cfg := exportWriteTestConfig(route)
			cfg.ExportWriteStallTimeout = d
			reports := exportWriteTestReports(&cfg)
			type result struct {
				calls   []exportWriteTestCall
				elapsed time.Duration
				err     error
			}
			done := make(chan result, 1)
			block := bytes.Repeat([]byte("s"), 64<<10)
			exportWriteTestStream(&cfg, route, func(_ context.Context, w io.Writer, flush func() error) (err error) {
				start := time.Now()
				recorded := &exportWriteRecordingWriter{writer: w}
				defer func() { done <- result{calls: recorded.calls, elapsed: time.Since(start), err: err} }()
				for i := range 128 {
					if _, err = recorded.Write(block); err != nil {
						return err
					}
					if (i+1)%4 == 0 {
						if err = flush(); err != nil {
							return err
						}
					}
				}
				return nil
			})
			srv, returned := exportWriteTestServer(t, cfg, 0)
			conn, resp := exportWriteRawClient(t, srv, route, "acc_write_deadline", d, 0)
			if err := conn.SetReadDeadline(time.Now().Add(200 * d)); err != nil {
				t.Fatal(err)
			}
			var body bytes.Buffer
			piece := make([]byte, 256<<10)
			for {
				n, err := io.ReadFull(resp.Body, piece)
				body.Write(piece[:n])
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					// ReadFull returns UnexpectedEOF for a valid final short body
					// fragment; only Body's EOF proves the chunked terminator.
					if err == io.ErrUnexpectedEOF {
						var extra [1]byte
						if n, err := resp.Body.Read(extra[:]); n != 0 || err != io.EOF {
							t.Fatalf("missing chunked terminator: bytes=%d error=%v", n, err)
						}
					}
					break
				}
				if err != nil {
					t.Fatalf("read TCP smoke stream after %d bytes: %v", body.Len(), err)
				}
			}
			got := exportWriteAwait(t, done, 20*d, "TCP smoke callback")
			if got.err != nil {
				t.Fatalf("stream error = %v", got.err)
			}
			var longest time.Duration
			var backpressured int
			for _, call := range got.calls {
				if call.err != nil {
					t.Fatalf("write error = %v", call.err)
				}
				if elapsed := call.end.Sub(call.start); elapsed > longest {
					longest = elapsed
				}
				if call.end.Sub(call.start) > d/8 {
					backpressured++
				}
			}
			t.Logf("TCP smoke: largest write=%d longest write=%v writes longer than d/8=%d stream=%v (%.2f*d) body bytes=%d", len(block), longest, backpressured, got.elapsed, float64(got.elapsed)/float64(d), body.Len())
			if !bytes.Equal(body.Bytes(), bytes.Repeat(block, 128)) {
				t.Fatalf("body differs: bytes=%d, want %d", body.Len(), 128*len(block))
			}
			if len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
				t.Fatalf("transfer encoding = %v, want chunked", resp.TransferEncoding)
			}
			exportWriteAwait(t, returned, 20*d, "handler return")
			// Retain the standard report observation window; the larger socket
			// deadline only gives progressing writes more scheduling headroom.
			exportWriteNoReport(t, reports, d/10)
		})
	}
}

func exportWriteTestSlowPauses(t *testing.T, pauses int) {
	t.Helper()
	for _, route := range []string{"evacuation", "backup"} {
		t.Run(route, func(t *testing.T) {
			d := exportWriteTestIdle
			cfg := exportWriteTestConfig(route)
			reports := exportWriteTestReports(&cfg)
			block := bytes.Repeat([]byte("p"), 1<<10)
			exportWriteTestStream(&cfg, route, func(_ context.Context, w io.Writer, flush func() error) error {
				for range pauses {
					if _, err := w.Write(block); err != nil {
						return err
					}
					if err := flush(); err != nil {
						return err
					}
					time.Sleep(3 * d)
				}
				return nil
			})
			srv, returned := exportWriteTestServer(t, cfg, 0)
			client := &http.Client{Timeout: 40 * d}
			resp, err := client.Do(exportWriteTestRequest(exportWriteTestURL(srv, route, "acc_write_deadline"), route))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read full body and chunked terminator: %v", err)
			}
			if !bytes.Equal(body, bytes.Repeat(block, pauses)) {
				t.Fatalf("body differs: bytes=%d, want %d", len(body), pauses*len(block))
			}
			exportWriteAwait(t, returned, 20*d, "handler return")
			exportWriteNoReport(t, reports, d)
		})
	}
}

func TestExportWriteDeadlineSlowSource(t *testing.T) { exportWriteTestSlowPauses(t, 4) }
func TestExportWriteDeadlineSlowCommit(t *testing.T) { exportWriteTestSlowPauses(t, 1) }

func TestExportWriteDeadlineThroughMetricsWrapper(t *testing.T) {
	set := make(chan error, 1)
	metrics := newRuntimeMetrics()
	srv := httptest.NewServer(metrics.instrument(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		set <- http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute))
		w.WriteHeader(http.StatusNoContent)
	})))
	t.Cleanup(srv.Close)
	client := &http.Client{Timeout: 20 * exportWriteTestIdle}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := exportWriteAwait(t, set, 20*exportWriteTestIdle, "deadline through metrics wrapper"); err != nil {
		t.Fatalf("SetWriteDeadline through metrics wrapper: %v", err)
	}
}

func TestExportWriteDeadlineKeepAlive(t *testing.T) {
	for _, route := range []string{"evacuation", "backup"} {
		t.Run(route, func(t *testing.T) {
			cfg := exportWriteTestConfig(route)
			reports := exportWriteTestReports(&cfg)
			exportWriteTestStream(&cfg, route, func(_ context.Context, w io.Writer, _ func() error) error {
				_, err := io.WriteString(w, "complete")
				return err
			})
			metrics := newRuntimeMetrics()
			mux := http.NewServeMux()
			mux.Handle("/", metrics.instrument(apiMux(metrics.instrumentConfig(cfg))))
			mux.HandleFunc("/plain", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "complete")
			})
			srv, returned := exportWriteTestHandlerServer(t, mux, 0)
			transport := &http.Transport{MaxConnsPerHost: 1}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport, Timeout: 20 * exportWriteTestIdle}
			for i := range 2 {
				reuse := make(chan bool, 1)
				req := exportWriteTestRequest(exportWriteTestURL(srv, route, "acc_write_deadline"), route)
				if i == 1 {
					var err error
					req, err = http.NewRequest(http.MethodGet, srv.URL+"/plain", nil)
					if err != nil {
						t.Fatal(err)
					}
				}
				req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
					GotConn: func(info httptrace.GotConnInfo) { reuse <- info.Reused },
				}))
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil || string(body) != "complete" || resp.StatusCode != http.StatusOK {
					t.Fatalf("request %d: status=%d bytes=%d read error=%v", i, resp.StatusCode, len(body), readErr)
				}
				if reused := exportWriteAwait(t, reuse, 20*exportWriteTestIdle, "connection trace"); i == 1 && !reused {
					t.Fatal("second request did not reuse the export connection")
				}
				exportWriteAwait(t, returned, 20*exportWriteTestIdle, "handler return")
				if i == 0 {
					// The next handler never touches the guard. Success on the
					// reused connection after expiry pins net/http clearing the
					// export deadline when the first response completes.
					time.Sleep(3 * exportWriteTestIdle)
				}
			}
			exportWriteNoReport(t, reports, exportWriteTestIdle)
		})
	}
}

func TestExportWriteDeadlineFlushUnsupported(t *testing.T) {
	for _, route := range []string{"evacuation", "backup"} {
		t.Run(route, func(t *testing.T) {
			for _, mode := range []string{"unavailable", "failed", "finish_failed"} {
				t.Run(mode, func(t *testing.T) {
					sentinel := errors.New("export flush sentinel")
					cfg := exportWriteTestConfig(route)
					reports := exportWriteTestReports(&cfg)
					exportWriteTestStream(&cfg, route, func(_ context.Context, w io.Writer, flush func() error) error {
						if _, err := io.WriteString(w, "chunk"); err != nil {
							return err
						}
						if mode == "finish_failed" {
							return nil
						}
						return flush()
					})
					recorder := httptest.NewRecorder()
					var w http.ResponseWriter = backupNoFlusher{recorder}
					if mode != "unavailable" {
						w = backupFailedFlusher{recorder, sentinel}
					}
					req := exportWriteTestRequest("http://example.test/v1/accounts/acc_write_deadline:export-"+route, route)
					if route == "evacuation" {
						accountLifecycleHandler(cfg)(w, req)
					} else {
						accountBackupHandler(cfg)(w, req)
					}
					if recorder.Body.String() != "chunk" {
						t.Fatalf("body bytes = %d, want unchanged chunk", recorder.Body.Len())
					}
					if mode != "unavailable" {
						report := exportWriteAwait(t, reports, 20*exportWriteTestIdle, "flush failure report")
						if report.accountID != "acc_write_deadline" || report.err != sentinel {
							t.Fatalf("flush report account=%s error=%v, want unchanged sentinel", report.accountID, report.err)
						}
					}
					exportWriteNoReport(t, reports, exportWriteTestIdle)
				})
			}
		})
	}
}

type exportWriteErrorWriter struct{ err error }

func (w exportWriteErrorWriter) Write([]byte) (int, error) { return 3, w.err }

func TestExportWriteGuardWriteErrorCancels(t *testing.T) {
	for _, sentinel := range []error{errors.New("write sentinel"), os.ErrDeadlineExceeded} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			canceled := 0
			g := &exportWriteGuard{
				writer: exportWriteErrorWriter{sentinel},
				rc:     http.NewResponseController(httptest.NewRecorder()),
				idle:   exportWriteTestIdle,
				cancel: func() { canceled++ },
			}
			n, err := g.Write([]byte("partial"))
			if n != 3 || err != sentinel || canceled != 1 {
				t.Fatalf("Write = (%d, %v), cancel count=%d; want (3, identical sentinel), 1", n, err, canceled)
			}
		})
	}
}

func TestExportWriteStallTimeoutAboveControlPlaneIdle(t *testing.T) {
	body, err := os.ReadFile("../../infra/cloudflare/control-plane/src/account-backup-runtime.mjs")
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile(`(?m)^export const IDLE_TIMEOUT_MS = (\d[\d_]*);$`).FindAllSubmatch(body, -1)
	if len(matches) != 1 {
		t.Fatalf("IDLE_TIMEOUT_MS matches = %d, want exactly one", len(matches))
	}
	milliseconds, err := strconv.ParseInt(strings.ReplaceAll(string(matches[0][1]), "_", ""), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	idle := time.Duration(milliseconds) * time.Millisecond
	if defaultExportWriteStallTimeout <= idle || defaultExportWriteStallTimeout > idle+60*time.Second {
		t.Fatalf("cell timeout=%v, want control-plane idle %v < timeout <= %v", defaultExportWriteStallTimeout, idle, idle+60*time.Second)
	}
	if got := exportWriteStallTimeout(Config{}); got != defaultExportWriteStallTimeout {
		t.Fatalf("zero config timeout=%v, want default=%v", got, defaultExportWriteStallTimeout)
	}
	if got := exportWriteStallTimeout(Config{ExportWriteStallTimeout: -time.Second}); got != defaultExportWriteStallTimeout {
		t.Fatalf("negative config timeout=%v, want default=%v", got, defaultExportWriteStallTimeout)
	}
	if got := exportWriteStallTimeout(Config{ExportWriteStallTimeout: exportWriteTestIdle}); got != exportWriteTestIdle {
		t.Fatalf("positive override=%v, want %v", got, exportWriteTestIdle)
	}
}

func TestExportWriteNoServerWriteTimeout(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			literal, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			selector, ok := literal.Type.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Server" {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok || pkg.Name != "http" {
				return true
			}
			found++
			for _, element := range literal.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := pair.Key.(*ast.Ident); ok && key.Name == "WriteTimeout" {
					t.Errorf("http.Server sets WriteTimeout at %s", fset.Position(key.Pos()))
				}
			}
			return true
		})
	}
	if found == 0 {
		t.Fatal("guard found nothing")
	}
}

func TestExportWriteClosedPeerEndsExport(t *testing.T) {
	for _, route := range []string{"evacuation", "backup"} {
		t.Run(route, func(t *testing.T) {
			d := exportWriteTestIdle
			cfg := exportWriteTestConfig(route)
			reports := exportWriteTestReports(&cfg)
			done := make(chan struct{}, 1)
			exportWriteTestStream(&cfg, route, func(ctx context.Context, w io.Writer, flush func() error) error {
				defer func() { done <- struct{}{} }()
				block := bytes.Repeat([]byte("c"), 1<<10)
				for {
					if _, err := w.Write(block); err != nil {
						return err
					}
					if err := flush(); err != nil {
						return err
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(d / 10):
					}
				}
			})
			srv, returned := exportWriteTestServer(t, cfg, 0)
			conn, _ := exportWriteRawClient(t, srv, route, "acc_write_deadline", d, 0)
			closed := time.Now()
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			exportWriteAwait(t, done, 20*d, "closed-peer cancellation")
			if elapsed := time.Since(closed); elapsed > 20*d {
				t.Fatalf("closed peer took %v, want <= %v", elapsed, 20*d)
			}
			exportWriteAwait(t, returned, 20*d, "handler return")
			select {
			case <-reports:
			default:
			}
			exportWriteNoReport(t, reports, d)
		})
	}
}

type exportWriteDeadlineErrorWriter struct {
	*httptest.ResponseRecorder
	deadlineErr error
	writes      int
	flushes     int
	deadlines   int
}

func (w *exportWriteDeadlineErrorWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.ResponseRecorder.Write(p)
}

func (w *exportWriteDeadlineErrorWriter) SetWriteDeadline(time.Time) error {
	w.deadlines++
	return w.deadlineErr
}

func (w *exportWriteDeadlineErrorWriter) FlushError() error {
	w.flushes++
	return nil
}

func TestExportWriteGuardDeadlineErrorCancels(t *testing.T) {
	for _, operation := range []string{"write", "flush", "finish"} {
		t.Run(operation, func(t *testing.T) {
			sentinel := errors.New("set deadline sentinel")
			w := &exportWriteDeadlineErrorWriter{ResponseRecorder: httptest.NewRecorder(), deadlineErr: sentinel}
			canceled := 0
			g := newExportWriteGuard(w, exportWriteTestIdle, func() { canceled++ })
			var err error
			switch operation {
			case "write":
				var n int
				n, err = g.Write([]byte("untouched"))
				if n != 0 {
					t.Fatalf("write count=%d, want zero after setting deadline failed", n)
				}
			case "flush":
				err = g.Flush()
			case "finish":
				err = g.finish()
			}
			if err != sentinel || canceled != 1 || w.deadlines != 1 || w.writes != 0 || w.flushes != 0 {
				t.Fatalf("error=%v cancel=%d deadlines=%d writes=%d flushes=%d; want unchanged sentinel, 1, 1, 0, 0", err, canceled, w.deadlines, w.writes, w.flushes)
			}
		})
	}
}
