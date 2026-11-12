//go:build windows

package localcell

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows synthetic receipt reserves 22 localcell tests at budget/33 (60s),
// the child-client matrix at budget/15 (6s) per row for its two rows (12s), and
// the harness at 24s. Children share their parent's deadline; the aggregate
// contract caps the total at 96s.
const windowsSyntheticTestBudget = 90 * time.Second

func windowsSyntheticTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), windowsSyntheticTestBudget/33)
	t.Cleanup(func() {
		if ctx.Err() != nil {
			t.Error("synthetic Windows test exceeded budget")
		}
		cancel()
	})
	return ctx
}

func windowsPipeTestDeadline(ctx context.Context) time.Time {
	deadline, _ := ctx.Deadline()
	return deadline
}

// Never let a failed cancellation mutant strand test cleanup in Close's Wait.
func windowsPipeTestClose(ctx context.Context, t *testing.T, listener *windowsPipeListener) {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = listener.Close(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		listener.mu.Lock()
		for _, handle := range listener.handles {
			_ = windows.CancelIoEx(handle, nil)
		}
		listener.mu.Unlock()
		t.Error("pipe cleanup exceeded synthetic test budget")
	}
}

func windowsPipeTestControlStop(ctx context.Context, t *testing.T, c *Cell, stop func()) {
	t.Helper()
	c.serviceControlMu.Lock()
	listener, _ := c.serviceControlListener.(*windowsPipeListener)
	c.serviceControlMu.Unlock()
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		if listener != nil {
			windowsPipeTestClose(ctx, t, listener)
		}
		t.Error("service control shutdown exceeded synthetic budget")
	}
}

func windowsPipeTestRecord(t *testing.T) serviceControlEndpointRecord {
	t.Helper()
	name, err := newServiceControlPipeName("lc_test")
	if err != nil {
		t.Fatal("cannot generate pipe fixture")
	}
	start, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		t.Fatal("cannot identify pipe fixture")
	}
	return serviceControlEndpointRecord{PipeName: name, HostPID: os.Getpid(), HostStartIdentity: start}
}

func windowsPipeTestListener(ctx context.Context, t *testing.T) (*windowsPipeListener, serviceControlEndpointRecord) {
	t.Helper()
	record := windowsPipeTestRecord(t)
	listener, err := windowsListenPipe(record.PipeName)
	if err != nil {
		t.Fatal("cannot bind pipe fixture")
	}
	t.Cleanup(func() { windowsPipeTestClose(ctx, t, listener) })
	return listener, record
}

func windowsPipeTestPair(ctx context.Context, t *testing.T, listener *windowsPipeListener, record serviceControlEndpointRecord) (net.Conn, net.Conn) {
	t.Helper()
	conn, err := windowsDialPipe(ctx, record)
	if err != nil {
		t.Fatal("cannot dial pipe fixture")
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		server, err := listener.Accept()
		if err == nil {
			accepted <- server
		}
	}()
	select {
	case server := <-accepted:
		_ = conn.SetDeadline(windowsPipeTestDeadline(ctx))
		_ = server.SetDeadline(windowsPipeTestDeadline(ctx))
		t.Cleanup(func() { _ = conn.Close(); _ = server.Close() })
		return conn, server
	case <-ctx.Done():
		_ = conn.Close()
		t.Fatal("pipe accept did not finish")
	}
	return nil, nil
}

// These are pipe-handle policy witnesses, not a two-user access receipt.
// Microsoft Learn: Named Pipe Security and Access Rights; GetSecurityInfo.
func TestWindowsPrivatePipePolicy(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil {
		t.Fatal("cannot obtain pipe policy principal")
	}
	owner := "O:" + user.User.Sid.String()
	allow := "(A;;FA;;;" + user.User.Sid.String() + ")"
	foreign := "S-1-5-21-100001-100002-100003-100004"
	fixtures := []struct {
		name, sddl string
		valid      bool
	}{
		{"exact", owner + "D:P" + allow + "(A;;FA;;;SY)", true},
		{"missing_system", owner + "D:P" + allow, false},
		{"everyone", owner + "D:P" + allow + "(A;;FA;;;SY)(A;;FA;;;WD)", false},
		{"users", owner + "D:P" + allow + "(A;;FA;;;SY)(A;;FA;;;BU)", false},
		{"authenticated_users", owner + "D:P" + allow + "(A;;FA;;;SY)(A;;FA;;;AU)", false},
		{"administrators", owner + "D:P" + allow + "(A;;FA;;;SY)(A;;FA;;;BA)", false},
		{"foreign_surrogate", owner + "D:P" + allow + "(A;;FA;;;" + foreign + ")", false},
		{"foreign_owner", "O:" + foreign + "D:P" + allow + "(A;;FA;;;SY)", false},
		{"unprotected", owner + "D:" + allow + "(A;;FA;;;SY)", false},
		{"inherited", owner + "D:P(A;ID;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)", false},
		{"deny", owner + "D:P(D;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)", false},
		{"partial", owner + "D:P(A;;FR;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)", false},
		{"null", owner + "D:PNO_ACCESS_CONTROL", false},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			descriptor, err := windows.SecurityDescriptorFromString(fixture.sddl)
			if err != nil {
				t.Fatal("cannot construct pipe policy fixture")
			}
			err = windowsPipeValidateDescriptor(descriptor)
			if (err == nil) != fixture.valid {
				t.Fatal("pipe policy result differs from exact owner and SYSTEM policy")
			}
			if err != nil && (!errors.Is(err, ErrServiceControlRefused) || !strings.Contains(err.Error(), "pipe:policy")) {
				t.Fatal("pipe policy lost fixed refusal detail")
			}
		})
	}
	listener, _ := windowsPipeTestListener(ctx, t)
	if len(listener.handles) != 4 {
		t.Fatal("pipe instance count changed")
	}
	for _, handle := range listener.handles {
		if windowsPipeValidateHandle(handle) != nil {
			t.Fatal("created pipe policy readback refused")
		}
		var flags uint32
		if windowsPipeTestHandleInformation(handle, &flags) != nil || flags&windows.HANDLE_FLAG_INHERIT != 0 {
			t.Fatal("pipe handle is inheritable")
		}
	}
}

func TestWindowsPrivatePipeCreationAndSquatter(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	original := windowsCreateNamedPipe
	calls := 0
	windowsCreateNamedPipe = func(name *uint16, flags, mode, maxInstances, outSize, inSize, timeout uint32, attributes *windows.SecurityAttributes) (windows.Handle, error) {
		want := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
		if calls == 0 {
			want |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
		}
		if flags != want || mode != windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS || maxInstances != 4 || outSize != 64*1024 || inSize != 64*1024 || attributes == nil || attributes.InheritHandle != 0 || windowsPipeValidateDescriptor(attributes.SecurityDescriptor) != nil {
			t.Error("pipe create policy changed")
		}
		calls++
		return original(name, flags, mode, maxInstances, outSize, inSize, timeout, attributes)
	}
	t.Cleanup(func() { windowsCreateNamedPipe = original })
	listener, record := windowsPipeTestListener(ctx, t)
	windowsCreateNamedPipe = original
	if calls != 4 {
		t.Fatal("bounded instance pool not created")
	}
	second, err := windowsListenPipe(record.PipeName)
	if second != nil || err == nil || !strings.Contains(err.Error(), "pipe:first_instance") {
		t.Fatal("first-instance collision was joined")
	}
	// The child owns one of four allowed instances; a second raw instance
	// proves spare capacity before the first-instance reservation is attempted.
	squatter := windowsPipeTestRecord(t)
	command, input, output := windowsPipeFixtureCommand(ctx, t, "pipe-squatter", squatter.PipeName)
	windowsPipeFixtureReady(ctx, t, output)
	spare := windowsPipeRawHandle(t, squatter.PipeName, false)
	_ = windows.CloseHandle(spare)
	joined, joinErr := windowsListenPipe(squatter.PipeName)
	if joined != nil {
		windowsPipeTestClose(ctx, t, joined)
	}
	if joined != nil || joinErr == nil || joinErr.Error() != "localcell: service_control_unavailable: pipe:first_instance" {
		t.Fatal("separate-process one-instance squatter was joined or served")
	}
	_, _ = io.WriteString(input, "done\n")
	if command.Wait() != nil {
		t.Fatal("squatter cleanup failed")
	}
	client, server := windowsPipeTestPair(ctx, t, listener, record)
	if client.LocalAddr().String() != "pipe" || server.RemoteAddr().String() != "pipe" || listener.Addr().Network() != "pipe" {
		t.Fatal("address disclosed random endpoint")
	}
}

func TestWindowsPrivatePipePeerIdentity(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	for _, kind := range []string{"pid", "start", "open_refusal", "foreign_sid", "sid_query_refusal"} {
		t.Run(kind, func(t *testing.T) {
			record := windowsPipeTestRecord(t)
			windowsPipeRawServer(t, record)
			originalOpen, originalSID := windowsOpenProcess, windowsProcessTokenUser
			t.Cleanup(func() { windowsOpenProcess, windowsProcessTokenUser = originalOpen, originalSID })
			want := "peer:identity"
			switch kind {
			case "pid":
				record.HostPID++
			case "start":
				record.HostStartIdentity = "windows:0:1"
			case "open_refusal":
				windowsOpenProcess = func(uint32, bool, uint32) (windows.Handle, error) { return 0, windows.ERROR_ACCESS_DENIED }
				want = "peer:pid"
			case "foreign_sid":
				sid, err := windows.StringToSid("S-1-5-21-100001-100002-100003-100004")
				if err != nil {
					t.Fatal("cannot construct foreign SID surrogate")
				}
				windowsProcessTokenUser = func(windows.Handle) (*windows.SID, error) { return sid, nil }
				want = "peer:sid"
			case "sid_query_refusal":
				windowsProcessTokenUser = func(windows.Handle) (*windows.SID, error) { return nil, windows.ERROR_ACCESS_DENIED }
				want = "peer:sid"
			}
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			conn, err := windowsDialPipe(ctx, record)
			if conn != nil {
				_ = conn.Close()
				t.Fatal("unverified pipe peer accepted")
			}
			if !errors.Is(err, ErrServiceControlRefused) || !strings.Contains(err.Error(), want) {
				t.Fatal("peer refusal lost exact raising site")
			}
		})
	}
}

func TestWindowsPrivatePipeServerRefusesBeforeHTTP(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	for _, kind := range []string{"open_refusal", "foreign_sid"} {
		t.Run(kind, func(t *testing.T) {
			originalOpen, originalSID := windowsOpenProcess, windowsProcessTokenUser
			if kind == "open_refusal" {
				windowsOpenProcess = func(uint32, bool, uint32) (windows.Handle, error) { return 0, windows.ERROR_ACCESS_DENIED }
			} else {
				sid, err := windows.StringToSid("S-1-5-21-100001-100002-100003-100004")
				if err != nil {
					t.Fatal("cannot construct SID fixture")
				}
				windowsProcessTokenUser = func(windows.Handle) (*windows.SID, error) { return sid, nil }
			}
			t.Cleanup(func() { windowsOpenProcess, windowsProcessTokenUser = originalOpen, originalSID })
			// Bypass client verification only in this probe, isolating server rejection.
			name, err := newServiceControlPipeName("lc_test")
			if err != nil {
				t.Fatal("cannot name fixture")
			}
			listener, err := windowsListenPipe(name)
			if err != nil {
				t.Fatal("cannot listen")
			}
			defer func() { windowsPipeTestClose(ctx, t, listener) }()
			pointer, _ := windows.UTF16PtrFromString(name)
			handle, err := windows.CreateFile(pointer, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
			if err != nil {
				t.Fatal("cannot open server refusal probe")
			}
			client, err := windowsNewPipeConn(handle, false)
			if err != nil {
				_ = windows.CloseHandle(handle)
				t.Fatal("cannot wrap refusal probe")
			}
			defer func() { _ = client.Close() }()
			_ = client.SetDeadline(windowsPipeTestDeadline(ctx))
			_, _ = client.Write([]byte("POST /status HTTP/1.1\r\nHost: localcell\r\n\r\n"))
			buffer := make([]byte, 1)
			n, err := client.Read(buffer)
			if n != 0 || err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal("rejected peer was not closed before response")
			}
		})
	}
}

var windowsTestHandleCount = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")

func windowsPipeHandleCount(t *testing.T) uint32 {
	t.Helper()
	var count uint32
	result, _, _ := windowsTestHandleCount.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&count)))
	if result == 0 {
		t.Fatal("cannot count fixture handles")
	}
	return count
}

func TestWindowsPrivatePipeDeadlinesAndHandles(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	// Warm runtime and listener event allocation before measuring steady-state.
	listener, record := windowsPipeTestListener(ctx, t)
	client, server := windowsPipeTestPair(ctx, t, listener, record)
	_ = client.Close()
	_ = server.Close()
	windowsPipeTestClose(ctx, t, listener)
	// Three cycles distinguish repeated leaks from a process-wide allocation burst.
	// Scheduled waits total 0.84s, the original 20-iteration loop's share of the
	// 90s/33 budget, so the extra cycles do not tighten the synthetic deadline.
	const cycles, iterationsPerCycle = 3, 7
	samples := []uint32{windowsPipeHandleCount(t)}
	for cycle := 0; cycle < cycles; cycle++ {
		listener, record = windowsPipeTestListener(ctx, t)
		for iteration := 0; iteration < iterationsPerCycle; iteration++ {
			client, server := windowsPipeTestPair(ctx, t, listener, record)
			started := time.Now()
			deadline := 40 * time.Millisecond
			_ = client.SetReadDeadline(started.Add(deadline))
			result := make(chan error, 1)
			go func() {
				n, err := client.Read(make([]byte, 1))
				if n != 0 {
					err = errors.New("unexpected deadline bytes")
				}
				result <- err
			}()
			select {
			case err := <-result:
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatal("blocked read ignored deadline")
				}
			case <-time.After(max(time.Second, 25*deadline)):
				_ = client.Close()
				t.Fatal("read cancellation exceeded proportional bound")
			case <-ctx.Done():
				_ = client.Close()
				t.Fatal("deadline loop exceeded synthetic budget")
			}
			if time.Since(started) > max(time.Second, 25*deadline) {
				t.Fatal("read cancellation exceeded proportional bound")
			}
			// A fresh operation succeeds after clearing the deadline; cancellation did
			// not poison the handle or cancel the opposite direction.
			_ = client.SetReadDeadline(windowsPipeTestDeadline(ctx))
			if _, err := server.Write([]byte{7}); err != nil {
				t.Fatal("deadline canceled unrelated direction")
			}
			buffer := make([]byte, 1)
			if _, err := io.ReadFull(client, buffer); err != nil || buffer[0] != 7 {
				t.Fatal("deadline recovery failed")
			}
			_ = client.Close()
			_ = server.Close()
		}
		// Closing the listener also retires every persistent reservation/pool event.
		windowsPipeTestClose(ctx, t, listener)
		samples = append(samples, windowsPipeHandleCount(t))
	}
	if class := classifyPipeHandleGrowth(samples, iterationsPerCycle); class != "steady" && class != "bounded" {
		t.Logf("pipe handle growth class=%s", class)
		t.Fatal("pipe deadline loop leaked handles")
	}
}

func TestWindowsPrivatePipeDeadlineUpdateAndClose(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	listener, record := windowsPipeTestListener(ctx, t)
	client, server := windowsPipeTestPair(ctx, t, listener, record)
	finished := make(chan error, 1)
	go func() { _, err := client.Read(make([]byte, 1)); finished <- err }()
	_ = client.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	select {
	case err := <-finished:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal("deadline update did not cancel blocked read")
		}
	case <-time.After(time.Second):
		t.Fatal("deadline update blocked")
	case <-ctx.Done():
		t.Fatal("deadline update exceeded synthetic budget")
	}
	_ = client.SetDeadline(windowsPipeTestDeadline(ctx))
	go func() { _, err := client.Read(make([]byte, 1)); finished <- err }()
	_ = client.Close()
	select {
	case err := <-finished:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal("close did not cancel blocked read")
		}
	case <-time.After(time.Second):
		t.Fatal("close leaked read")
	case <-ctx.Done():
		t.Fatal("read close exceeded synthetic budget")
	}
	_ = server.Close()
	// Establish ERROR_IO_PENDING before invoking Close, without scheduler sleeps.
	// Calibrate on this exact ConnectNamedPipe/GetOverlappedResult cancellation seam.
	calibration, _, calibrationDone := windowsPipePendingConnect(t)
	t.Cleanup(func() { windowsPipeTestClose(ctx, t, calibration) })
	began := time.Now()
	for _, handle := range calibration.handles {
		_ = windows.CancelIoEx(handle, nil)
	}
	select {
	case <-calibrationDone:
	case <-ctx.Done():
		t.Fatal("pending-connect calibration stalled")
	}
	bound := max(100*time.Millisecond, 10*time.Since(began))
	windowsPipeTestClose(ctx, t, calibration)
	pending, _, _ := windowsPipePendingConnect(t)
	t.Cleanup(func() { windowsPipeTestClose(ctx, t, pending) })
	accepting := make(chan error, 1)
	go func() { _, err := pending.Accept(); accepting <- err }()
	closed := make(chan error, 1)
	go func() { closed <- pending.Close() }()
	// Both assertions are independent of Close's implementation returning.
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal("pending-connect Close failed")
		}
	case <-time.After(bound):
		for _, handle := range pending.handles {
			_ = windows.CancelIoEx(handle, nil)
		}
		t.Error("pending-connect Close exceeded calibrated proportional bound")
	case <-ctx.Done():
		for _, handle := range pending.handles {
			_ = windows.CancelIoEx(handle, nil)
		}
		t.Error("pending-connect Close exceeded synthetic budget")
	}
	select {
	case err := <-accepting:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal("closed accept lost net.ErrClosed")
		}
	case <-ctx.Done():
		t.Fatal("close leaked accept")
	}
	closedAgain := make(chan error, 1)
	go func() { closedAgain <- pending.Close() }()
	select {
	case err := <-closedAgain:
		if err != nil {
			t.Fatal("listener close is not idempotent")
		}
	case <-ctx.Done():
		t.Fatal("idempotent listener close exceeded budget")
	}
	if client.Close() != nil {
		t.Fatal("connection close is not idempotent")
	}
}

func TestWindowsPrivatePipeConcurrentClients(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	listener, record := windowsPipeTestListener(ctx, t)
	results := make(chan error, 8)
	var servers sync.WaitGroup
	go func() {
		for count := 0; count < 8; count++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			servers.Add(1)
			go func() {
				defer servers.Done()
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(windowsPipeTestDeadline(ctx))
				buffer := make([]byte, 4)
				if _, err := io.ReadFull(conn, buffer); err == nil {
					_, _ = conn.Write(buffer)
				}
			}()
		}
	}()
	for count := 0; count < 8; count++ {
		go func() {
			conn, err := windowsDialPipe(ctx, record)
			if err != nil {
				results <- err
				return
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(windowsPipeTestDeadline(ctx))
			_, err = conn.Write([]byte("ping"))
			if err == nil {
				buffer := make([]byte, 4)
				_, err = io.ReadFull(conn, buffer)
				if err == nil && string(buffer) != "ping" {
					err = errors.New("unexpected echo")
				}
			}
			results <- err
		}()
	}
	for count := 0; count < 8; count++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal("concurrent pipe exchange failed")
			}
		case <-ctx.Done():
			t.Fatal("bounded instance pool stalled")
		}
	}
	done := make(chan struct{})
	go func() { servers.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("concurrent servers exceeded budget")
	}
	if len(listener.handles) != 4 {
		t.Fatal("concurrency expanded instance bound")
	}
}

func TestWindowsServiceControlAuthentication(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	lease, err := c.acquireServiceLease(d)
	if err != nil {
		t.Fatal("cannot acquire control fixture lease")
	}
	defer func() { _ = lease.Close() }()
	commands := make(chan serviceCommand)
	closeControl, err := c.serveServiceControl(commands)
	if err != nil {
		t.Fatal("cannot serve control fixture")
	}
	defer windowsPipeTestControlStop(ctx, t, c, closeControl)
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dialServiceControl(ctx) }}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	for _, token := range []string{"", "wrong"} {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localcell/status", nil)
		if err != nil {
			t.Fatal("cannot construct auth probe")
		}
		request.Header.Set("Authorization", token)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("cannot probe pipe authentication")
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 128))
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusUnauthorized || string(body) != "service_control_refused\n" {
			t.Fatal("wrong or absent token changed fixed HTTP refusal")
		}
		select {
		case <-commands:
			t.Fatal("unauthenticated command reached supervisor")
		default:
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for {
			select {
			case command := <-commands:
				command.reply <- serviceReply{status: ServiceStatus{Generation: d.Payload.Generation, DesiredState: command.action}}
			case <-ctx.Done():
				return
			}
		}
	}()
	for _, action := range []string{"status", "observe", "stop"} {
		requestContext, requestCancel := context.WithTimeout(ctx, 2*time.Second)
		status, err := c.Control(requestContext, action)
		requestCancel()
		if err != nil || status.Generation != d.Payload.Generation || status.DesiredState != action {
			t.Fatal("authenticated pipe command or response failed")
		}
	}
}

func TestWindowsPipeProcessFixture(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	marker := -1
	for i, arg := range os.Args {
		if arg == "--" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	args := os.Args[marker+1:]
	if len(args) < 2 {
		return
	}
	switch args[0] {
	case "pipe-client":
		if len(args) != 4 {
			os.Exit(41)
		}
		pid, err := strconv.Atoi(args[2])
		if err != nil {
			os.Exit(42)
		}
		conn, err := windowsDialPipe(ctx, serviceControlEndpointRecord{PipeName: args[1], HostPID: pid, HostStartIdentity: args[3]})
		if err != nil {
			os.Exit(43)
		}
		_ = conn.SetDeadline(windowsPipeTestDeadline(ctx))
		_, err = conn.Write([]byte("child"))
		if err != nil {
			os.Exit(44)
		}
		data := make([]byte, 5)
		_, err = io.ReadFull(conn, data)
		_ = conn.Close()
		if err != nil || string(data) != "child" {
			os.Exit(45)
		}
		os.Exit(0)
	case "pipe-decoy", "pipe-squatter":
		handle := windowsPipeRawHandle(t, args[1], true)
		defer func() { _ = windows.CloseHandle(handle) }()
		if args[0] == "pipe-squatter" {
			_, _ = io.WriteString(os.Stdout, "ready\n")
			done := make(chan struct{})
			go func() { _, _ = bufio.NewReader(os.Stdin).ReadString('\n'); close(done) }()
			select {
			case <-done:
			case <-ctx.Done():
				os.Exit(46)
			}
			os.Exit(0)
		}
		event, err := windows.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			os.Exit(47)
		}
		operation := windows.Overlapped{HEvent: event}
		err = windows.ConnectNamedPipe(handle, &operation)
		if !errors.Is(err, windows.ERROR_IO_PENDING) {
			os.Exit(48)
		}
		_, _ = io.WriteString(os.Stdout, "ready\n")
		err = windowsPipeAwaitOperation(ctx, handle, &operation, nil)
		if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			os.Exit(48)
		}
		conn, err := windowsNewPipeConn(handle, true)
		if err != nil {
			os.Exit(49)
		}
		_ = conn.SetReadDeadline(windowsPipeTestDeadline(ctx))
		// Return only a count. Never expose fixture bearer bytes or request text.
		count, _ := conn.Read(make([]byte, 8192))
		_, _ = fmt.Fprintf(os.Stdout, "%d\n", count)
		_ = conn.Close()
		os.Exit(0)
	}
}

func windowsPipeFixtureCommand(ctx context.Context, t *testing.T, args ...string) (*exec.Cmd, io.WriteCloser, *bufio.Reader) {
	t.Helper()
	c, _ := unitCell(t)
	childDeadline := windowsPipeTestDeadline(ctx).Add(-windowsSyntheticTestBudget / 300)
	childContext, cancel := context.WithDeadline(ctx, childDeadline)
	t.Cleanup(cancel)
	command := exec.CommandContext(childContext, os.Args[0], append([]string{"-test.run=^TestWindowsPipeProcessFixture$", "--"}, args...)...)
	command.WaitDelay = windowsSyntheticTestBudget / 300
	command.Env, _ = c.baseEnvironment()
	command.Env = appendEnvironment(command.Env, "WITSELF_HOME="+c.path("home"), "DSH_HOME="+c.path("home"))
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal("cannot construct pipe child input")
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal("cannot construct pipe child output")
	}
	command.Stderr = io.Discard
	if command.Start() != nil {
		t.Fatal("cannot start pipe fixture process")
	}
	t.Cleanup(func() { _ = input.Close(); _ = command.Process.Kill(); _ = command.Wait() })
	return command, input, bufio.NewReader(output)
}

func TestWindowsPrivatePipeAcrossProcesses(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	listener, record := windowsPipeTestListener(ctx, t)
	command, _, _ := windowsPipeFixtureCommand(ctx, t, "pipe-client", record.PipeName, strconv.Itoa(record.HostPID), record.HostStartIdentity)
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-ctx.Done():
		t.Fatal("cannot accept fixture child within deadline")
	}
	_ = conn.SetDeadline(windowsPipeTestDeadline(ctx))
	data := make([]byte, 5)
	if _, err := io.ReadFull(conn, data); err != nil || string(data) != "child" {
		t.Fatal("child pipe request failed")
	}
	if _, err := conn.Write(data); err != nil {
		t.Fatal("child pipe reply failed")
	}
	_ = conn.Close()
	if command.Wait() != nil {
		t.Fatal("child pipe identity or exchange failed")
	}
}

func TestWindowsPrivatePipeDecoyServer(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	c, _ := unitCell(t)
	descriptor := prepareSupervisorTest(t, c) // Generates the fixture bearer; never print it.
	lease, err := c.acquireServiceLease(descriptor)
	if err != nil {
		t.Fatal("cannot acquire decoy fixture lease")
	}
	defer func() { _ = lease.Close() }()
	var originalLease serviceLeaseRecord
	if readJSON(c.path("service", "lease.json"), &originalLease) != nil {
		t.Fatal("cannot read fixture lease")
	}
	for _, mismatch := range []string{"pid", "start"} {
		t.Run(mismatch, func(t *testing.T) {
			name, err := newServiceControlPipeName(descriptor.InstallationID)
			if err != nil {
				t.Fatal("cannot name decoy fixture")
			}
			record, err := c.publishServiceControlEndpoint(name)
			if err != nil {
				t.Fatal("cannot publish decoy fixture")
			}
			command, _, output := windowsPipeFixtureCommand(ctx, t, "pipe-decoy", name)
			windowsPipeFixtureReady(ctx, t, output)
			staleLease := originalLease
			if mismatch == "start" {
				record.HostPID, staleLease.PID = command.Process.Pid, command.Process.Pid
				record.HostStartIdentity, staleLease.StartIdentity = "windows:0:1", "windows:0:1"
				if writeAtomic(c.path("service", "lease.json"), staleLease, nil) != nil || writeAtomic(c.path("service", "control-endpoint.json"), record, nil) != nil {
					t.Fatal("cannot publish stale incarnation fixture")
				}
			}
			_, controlErr := c.Control(ctx, "status")
			count := make(chan int, 1)
			go func() {
				line, err := output.ReadString('\n')
				n, parseErr := strconv.Atoi(strings.TrimSuffix(line, "\n"))
				if err != nil || parseErr != nil {
					n = -1
				}
				count <- n
			}()
			var received int
			select {
			case received = <-count:
			case <-ctx.Done():
				t.Fatal("decoy byte-count receipt exceeded budget")
			}
			if controlErr == nil || controlErr.Error() != windowsControlRefusal("peer:identity").Error() || !errors.Is(controlErr, ErrServiceControlRefused) || received != 0 {
				t.Fatal("real Control must refuse exact peer identity AND disclose zero bytes")
			}
			if command.Wait() != nil {
				t.Fatal("decoy cleanup failed")
			}
			if writeAtomic(c.path("service", "lease.json"), originalLease, nil) != nil {
				t.Fatal("cannot restore fixture lease")
			}
		})
	}
	commands := make(chan serviceCommand)
	stop, err := c.serveServiceControl(commands)
	if err != nil {
		t.Fatal("cannot serve valid control exchange")
	}
	defer windowsPipeTestControlStop(ctx, t, c, stop)
	go func() {
		select {
		case command := <-commands:
			command.reply <- serviceReply{status: ServiceStatus{Generation: descriptor.Payload.Generation, DesiredState: command.action}}
		case <-ctx.Done():
		}
	}()
	status, err := c.Control(ctx, "status")
	if err != nil || status.Generation != descriptor.Payload.Generation || status.DesiredState != "status" {
		t.Fatal("valid generated-bearer Control exchange failed")
	}
}

func windowsPipeFixtureReady(ctx context.Context, t *testing.T, output *bufio.Reader) {
	t.Helper()
	ready := make(chan bool, 1)
	go func() { line, err := output.ReadString('\n'); ready <- err == nil && line == "ready\n" }()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("pipe child did not bind")
		}
	case <-ctx.Done():
		t.Fatal("pipe child readiness exceeded budget")
	}
}

// Client-side injectable refusals use a raw, unserviced server so the server's
// own injected peer check cannot mask the client-side raising site.
func windowsPipeRawServer(t *testing.T, record serviceControlEndpointRecord) {
	t.Helper()
	handle := windowsPipeRawHandle(t, record.PipeName, true)
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
}

func windowsPipeRawHandle(t *testing.T, name string, first bool) windows.Handle {
	t.Helper()
	descriptor, err := windowsPipeDescriptor()
	if err != nil {
		t.Fatal("cannot construct raw pipe policy")
	}
	pointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal("cannot encode raw pipe name")
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	flags := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	handle, err := windows.CreateNamedPipe(pointer, flags, windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, 4, 64*1024, 64*1024, 0, &attributes)
	runtime.KeepAlive(descriptor)
	if err != nil {
		t.Fatal("cannot bind raw pipe fixture")
	}
	return handle
}

func windowsPipeAwaitOperation(ctx context.Context, handle windows.Handle, operation *windows.Overlapped, buffer []byte) error {
	done := make(chan error, 1)
	go func() {
		var transferred uint32
		err := windows.GetOverlappedResult(handle, operation, &transferred, true)
		_ = windows.CloseHandle(operation.HEvent)
		runtime.KeepAlive(buffer)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = windows.CancelIoEx(handle, operation)
		// The goroutine owns operation through cancellation completion; callers
		// terminate the child or close the connection on this bounded failure.
		return ctx.Err()
	}
}

func windowsPipePendingConnect(t *testing.T) (*windowsPipeListener, windows.Handle, <-chan struct{}) {
	t.Helper()
	record := windowsPipeTestRecord(t)
	handle := windowsPipeRawHandle(t, record.PipeName, true)
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("cannot create pending connect event")
	}
	operation := &windows.Overlapped{HEvent: event}
	if err := windows.ConnectNamedPipe(handle, operation); !errors.Is(err, windows.ERROR_IO_PENDING) {
		t.Fatal("connect fixture was not pending")
	}
	listener := &windowsPipeListener{handles: []windows.Handle{handle}, active: make(map[*windowsPipeConn]bool), accepts: make(chan net.Conn), failures: make(chan error, 1), stop: make(chan struct{})}
	done := make(chan struct{})
	listener.workers.Add(1)
	go func() {
		defer listener.workers.Done()
		var transferred uint32
		_ = windows.GetOverlappedResult(handle, operation, &transferred, true)
		_ = windows.CloseHandle(event)
		close(done)
	}()
	return listener, handle, done
}

var windowsGetHandleInformation = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetHandleInformation")

func windowsPipeTestHandleInformation(handle windows.Handle, flags *uint32) error {
	result, _, _ := windowsGetHandleInformation.Call(uintptr(handle), uintptr(unsafe.Pointer(flags)))
	if result == 0 {
		return errors.New("handle information unavailable")
	}
	return nil
}

func TestWindowsPrivatePipeWriteDeadline(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	listener, record := windowsPipeTestListener(ctx, t)
	client, server := windowsPipeTestPair(ctx, t, listener, record)
	defer func() { _ = client.Close(); _ = server.Close() }()
	deadline := 40 * time.Millisecond
	started := time.Now()
	if client.SetWriteDeadline(started.Add(deadline)) != nil {
		t.Fatal("cannot set write deadline")
	}
	// Exceed the advertised 64 KiB pipe buffer with no reader. Completion must
	// report the actual short count and cancellation, not claim a whole write.
	buffer := make([]byte, 4*1024*1024)
	type writeResult struct {
		n   int
		err error
	}
	done := make(chan writeResult, 1)
	go func() { n, err := client.Write(buffer); done <- writeResult{n, err} }()
	select {
	case result := <-done:
		if !errors.Is(result.err, os.ErrDeadlineExceeded) || result.n < 0 || result.n >= len(buffer) {
			t.Fatal("blocked write did not report deadline and honest count")
		}
	case <-time.After(max(time.Second, 25*deadline)):
		_ = client.Close()
		t.Fatal("write cancellation exceeded proportional bound")
	case <-ctx.Done():
		_ = client.Close()
		t.Fatal("write cancellation exceeded synthetic budget")
	}
	if time.Since(started) > max(time.Second, 25*deadline) {
		t.Fatal("write cancellation exceeded proportional bound")
	}
}

func TestWindowsServiceControlEndpointCleanup(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	for _, replacement := range []bool{false, true} {
		name := "owned"
		if replacement {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := unitCell(t)
			descriptor := prepareSupervisorTest(t, c)
			lease, err := c.acquireServiceLease(descriptor)
			if err != nil {
				t.Fatal("cannot acquire cleanup fixture lease")
			}
			defer func() { _ = lease.Close() }()
			listener, err := c.listenServiceControl()
			if err != nil {
				t.Fatal("cannot create cleanup fixture listener")
			}
			defer func() { windowsPipeTestClose(ctx, t, listener.(*windowsPipeListener)) }()
			owned, err := c.readServiceControlEndpoint()
			if err != nil {
				t.Fatal("cannot read owned endpoint fixture")
			}
			successor := owned
			if replacement {
				successor.PipeName, err = newServiceControlPipeName(descriptor.InstallationID)
				if err != nil {
					t.Fatal("cannot name successor fixture")
				}
				if writeAtomic(c.path("service", "control-endpoint.json"), successor, nil) != nil {
					t.Fatal("cannot publish replacement fixture")
				}
			}
			removed := make(chan error, 1)
			go func() { removed <- c.removeServiceControlEndpoint() }()
			select {
			case err = <-removed:
			case <-ctx.Done():
				t.Fatal("endpoint cleanup exceeded synthetic budget")
			}
			if replacement {
				if !errors.Is(err, ErrServiceControlRefused) {
					t.Fatal("cleanup did not refuse replacement record")
				}
				after, readErr := c.readServiceControlEndpointRecord()
				if readErr != nil || after != successor {
					t.Fatal("old host removed replacement endpoint record")
				}
			} else {
				if err != nil {
					t.Fatal("owned endpoint cleanup refused")
				}
				if _, statErr := os.Lstat(c.path("service", "control-endpoint.json")); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("owned endpoint record survived cleanup")
				}
			}
			accepted := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if conn != nil {
					_ = conn.Close()
					err = errors.New("unexpected cleanup accept")
				}
				accepted <- err
			}()
			select {
			case err := <-accepted:
				if !errors.Is(err, net.ErrClosed) {
					t.Fatal("cleanup did not close owned listener before record removal")
				}
			case <-ctx.Done():
				t.Fatal("cleanup accept exceeded synthetic budget")
			}
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			if conn, dialErr := windowsDialPipe(ctx, owned); conn != nil || dialErr == nil {
				if conn != nil {
					_ = conn.Close()
				}
				t.Fatal("owned pipe survived cleanup")
			}
		})
	}
}

func TestWindowsPrivatePipeFirstInstanceRefusals(t *testing.T) {
	_ = windowsSyntheticTestContext(t)
	original := windowsCreateNamedPipe
	t.Cleanup(func() { windowsCreateNamedPipe = original })
	for _, fixture := range []struct {
		name string
		err  error
	}{{"access_denied", windows.ERROR_ACCESS_DENIED}, {"pipe_busy", windows.ERROR_PIPE_BUSY}} {
		t.Run(fixture.name, func(t *testing.T) {
			calls := 0
			windowsCreateNamedPipe = func(_ *uint16, flags, _, _, _, _, _ uint32, _ *windows.SecurityAttributes) (windows.Handle, error) {
				calls++
				if flags&windows.FILE_FLAG_FIRST_PIPE_INSTANCE == 0 {
					t.Error("first instance lost reservation flag")
				}
				return windows.InvalidHandle, fixture.err
			}
			name, err := newServiceControlPipeName("lc_test")
			if err != nil {
				t.Fatal("cannot name first-instance probe")
			}
			listener, err := windowsListenPipe(name)
			if listener != nil || err == nil || !strings.Contains(err.Error(), "service_control_unavailable: pipe:first_instance") || calls != 1 {
				t.Fatal("first-instance refusal retried or joined another listener")
			}
		})
	}
}

func TestWindowsServiceControlHostedHasNoPlatformEffects(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	t.Setenv("WITSELF_RUNTIME", "")
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	originalBoot, originalOpen, originalPipe := windowsQueryBootEnvironment, windowsOpenProcess, windowsCreateNamedPipe
	t.Cleanup(func() {
		windowsQueryBootEnvironment, windowsOpenProcess, windowsCreateNamedPipe = originalBoot, originalOpen, originalPipe
	})
	calls := 0
	windowsQueryBootEnvironment = func() (uint32, uint32, []byte) { calls++; return 1, 0, nil }
	windowsOpenProcess = func(uint32, bool, uint32) (windows.Handle, error) { calls++; return 0, windows.ERROR_ACCESS_DENIED }
	windowsCreateNamedPipe = func(*uint16, uint32, uint32, uint32, uint32, uint32, uint32, *windows.SecurityAttributes) (windows.Handle, error) {
		calls++
		return 0, windows.ERROR_ACCESS_DENIED
	}
	cell := &Cell{}
	if _, err := cell.Control(ctx, "status"); !errors.Is(err, ErrServiceControlRefused) {
		t.Fatal("hosted control did not refuse before platform work")
	}
	if cell.Run(ctx, SupervisorPolicy{}) == nil {
		t.Fatal("hosted foreground run did not refuse")
	}
	if calls != 0 {
		t.Fatal("empty runtime selector reached Windows platform APIs")
	}
}

func TestWindowsServiceControlPublicationAfterFirstInstance(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	c, _ := unitCell(t)
	descriptor := prepareSupervisorTest(t, c)
	lease, err := c.acquireServiceLease(descriptor)
	if err != nil {
		t.Fatal("cannot acquire publication fixture lease")
	}
	defer func() { _ = lease.Close() }()
	original := windowsCreateNamedPipe
	t.Cleanup(func() { windowsCreateNamedPipe = original })
	calls := 0
	windowsCreateNamedPipe = func(*uint16, uint32, uint32, uint32, uint32, uint32, uint32, *windows.SecurityAttributes) (windows.Handle, error) {
		calls++
		return windows.InvalidHandle, windows.ERROR_ACCESS_DENIED
	}
	listener, err := c.listenServiceControl()
	if listener != nil || err == nil || err.Error() != "localcell: service_control_unavailable: pipe:first_instance" || calls != 1 {
		t.Fatal("failed first-create did not refuse at exact boundary")
	}
	if _, err := os.Lstat(c.path("service", "control-endpoint.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed first-create published an endpoint")
	}
	windowsCreateNamedPipe = original
	listener, err = c.listenServiceControl()
	if err != nil {
		t.Fatal("valid first-create did not publish")
	}
	t.Cleanup(func() { windowsPipeTestClose(ctx, t, listener.(*windowsPipeListener)) })
	record, err := c.readServiceControlEndpoint()
	if err != nil || record.HostPID != os.Getpid() || record.Generation != descriptor.Payload.Generation || !validServiceControlPipeName(record.PipeName, descriptor.InstallationID) {
		t.Fatal("successful first-create publication was not valid")
	}
}

func TestWindowsPrivatePipeDisconnectedPeerEOF(t *testing.T) {
	ctx := windowsSyntheticTestContext(t)
	for _, code := range []error{windows.ERROR_BROKEN_PIPE, windows.ERROR_PIPE_NOT_CONNECTED} {
		if !errors.Is(windowsPipeIOError(code), io.EOF) {
			t.Fatal("disconnected pipe mapping lost EOF")
		}
	}
	if err := windowsPipeIOError(windows.ERROR_NO_DATA); errors.Is(err, io.EOF) || err == nil || err.Error() != windowsControlRefusal("pipe:io").Error() {
		t.Fatal("closing pipe must retain fixed non-EOF refusal")
	}
	t.Run("read_no_data_refused", func(t *testing.T) {
		listener, record := windowsPipeTestListener(ctx, t)
		client, _ := windowsPipeTestPair(ctx, t, listener, record)
		original := windowsPipeReadFile
		t.Cleanup(func() { windowsPipeReadFile = original })
		calls := 0
		windowsPipeReadFile = func(handle windows.Handle, buffer []byte, transferred *uint32, operation *windows.Overlapped) error {
			calls++
			if handle != client.(*windowsPipeConn).handle || len(buffer) != 1 || transferred == nil || operation == nil || operation.HEvent == 0 {
				t.Error("Read did not supply the native read operation")
			}
			return windows.ERROR_NO_DATA
		}
		n, err := client.Read(make([]byte, 1))
		if calls != 1 || n != 0 || !errors.Is(err, ErrServiceControlRefused) || errors.Is(err, io.EOF) || err.Error() != "localcell: service_control_refused: pipe:io" {
			t.Fatal("native closing-pipe Read did not retain the fixed non-EOF refusal")
		}
	})
	for _, kind := range []string{"disconnect", "close_handle"} {
		t.Run(kind, func(t *testing.T) {
			record := windowsPipeTestRecord(t)
			handle := windowsPipeRawHandle(t, record.PipeName, true)
			defer func() {
				if handle != windows.InvalidHandle {
					_ = windows.CloseHandle(handle)
				}
			}()
			client, err := windowsDialPipe(ctx, record)
			if err != nil {
				t.Fatal("cannot dial EOF fixture")
			}
			defer func() { _ = client.Close() }()
			_ = client.SetReadDeadline(windowsPipeTestDeadline(ctx))
			event, err := windows.CreateEvent(nil, 1, 0, nil)
			if err != nil {
				t.Fatal("cannot create EOF fixture event")
			}
			eventOwned := true
			defer func() {
				if eventOwned {
					_ = windows.CloseHandle(event)
				}
			}()
			operation := windows.Overlapped{HEvent: event}
			if err := windows.ConnectNamedPipe(handle, &operation); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
				t.Fatal("EOF fixture peer was not connected")
			}
			if kind == "disconnect" {
				if windows.DisconnectNamedPipe(handle) != nil {
					t.Fatal("cannot disconnect EOF fixture peer")
				}
			} else {
				if windows.CloseHandle(handle) != nil {
					t.Fatal("cannot close EOF fixture peer handle")
				}
				handle = windows.InvalidHandle
			}
			// Observe the real native error separately from our Go mapping, without
			// printing any pipe name, identity, request bytes, or raw Windows message.
			pipe := client.(*windowsPipeConn)
			operation = windows.Overlapped{HEvent: event}
			var transferred uint32
			buffer := make([]byte, 1)
			nativeErr := windows.ReadFile(pipe.handle, buffer, &transferred, &operation)
			if errors.Is(nativeErr, windows.ERROR_IO_PENDING) {
				eventOwned = false // The waiter retains the event and buffer until completion.
				nativeErr = windowsPipeAwaitOperation(ctx, pipe.handle, &operation, buffer)
			}
			runtime.KeepAlive(buffer)
			code, ok := nativeErr.(syscall.Errno)
			if ok {
				t.Logf("service_pipe_eof operation=%s error_code=%d", kind, uint32(code))
			}
			if !ok || (code != windows.ERROR_BROKEN_PIPE && code != windows.ERROR_PIPE_NOT_CONNECTED) {
				t.Fatal("real disconnected peer did not produce a documented EOF code")
			}
			if n, err := client.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatal("real disconnected peer Read did not return EOF")
			}
		})
	}
}
