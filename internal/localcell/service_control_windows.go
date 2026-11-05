//go:build windows

package localcell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsPipeInstances = 4
const windowsPipeFullAccess = uint32(0x001f01ff)

var windowsCreateNamedPipe = windows.CreateNamedPipe
var windowsPipeReadFile = windows.ReadFile

func windowsControlRefusal(step string) error {
	return &serviceControlRefusal{step: step}
}

func serviceControlCapability() error {
	if _, err := serviceBootID(); err != nil {
		return fmt.Errorf("%w: service_boot_identity_unavailable", ErrServiceManagerUnavailable)
	}
	return nil
}

func (c *Cell) validateServiceControlEndpoint() error {
	_, err := c.readServiceControlEndpoint()
	return err
}

func (c *Cell) listenServiceControl() (net.Listener, error) {
	d, err := c.ServiceDescriptor()
	if err != nil {
		return nil, serviceFailureAt(windowsControlRefusal("endpoint:record"), "endpoint_descriptor")
	}
	name, err := newServiceControlPipeName(d.InstallationID)
	if err != nil {
		return nil, windowsControlRefusal("pipe:name")
	}
	listener, err := windowsListenPipe(name)
	if err != nil {
		return nil, err
	}
	record, err := c.publishServiceControlEndpoint(name)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	c.serviceControlMu.Lock()
	c.serviceControlOwned, c.serviceControlListener = &record, listener
	c.serviceControlMu.Unlock()
	return listener, nil
}

func (c *Cell) removeServiceControlEndpoint() error {
	c.serviceControlMu.Lock()
	owned, listener := c.serviceControlOwned, c.serviceControlListener
	c.serviceControlOwned, c.serviceControlListener = nil, nil
	c.serviceControlMu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	if owned == nil {
		return nil
	}
	return c.removeOwnedServiceControlEndpoint(*owned)
}

func (c *Cell) dialServiceControl(ctx context.Context) (net.Conn, error) {
	record, err := c.readServiceControlEndpoint()
	if err != nil {
		return nil, err
	}
	return windowsDialPipe(ctx, record)
}

// The pipe policy intentionally does not call filesystem path/type validators.
// Microsoft Learn: Named Pipe Security and Access Rights; CreateNamedPipeW
// function (winbase.h); GetSecurityInfo function (aclapi.h).
func windowsPipeDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return nil, windowsControlRefusal("pipe:policy")
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)")
	if err != nil {
		return nil, windowsControlRefusal("pipe:policy")
	}
	return sd, nil
}

func windowsPipeValidateDescriptor(sd *windows.SECURITY_DESCRIPTOR) error {
	refuse := func() error { return windowsControlRefusal("pipe:policy") }
	if sd == nil || !sd.IsValid() {
		return refuse()
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return refuse()
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return refuse()
	}
	owner, defaulted, err := sd.Owner()
	if err != nil || defaulted || owner == nil || !owner.Equals(user.User.Sid) {
		return refuse()
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 || control&windows.SE_DACL_DEFAULTED != 0 {
		return refuse()
	}
	acl, defaulted, err := sd.DACL()
	if err != nil || defaulted || acl == nil || acl.AceCount != 2 {
		return refuse()
	}
	var foundUser, foundSystem bool
	for index := uint32(0); index < 2; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, index, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 {
			return refuse()
		}
		if uint32(ace.Mask) != windowsPipeFullAccess {
			return refuse()
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || uintptr(ace.Header.AceSize) != unsafe.Offsetof(ace.SidStart)+uintptr(sid.Len()) {
			return refuse()
		}
		switch {
		case sid.Equals(user.User.Sid) && !foundUser:
			foundUser = true
		case sid.Equals(system) && !foundSystem:
			foundSystem = true
		default:
			return refuse()
		}
	}
	if !foundUser || !foundSystem {
		return refuse()
	}
	return nil
}

func windowsPipeValidateHandle(handle windows.Handle) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return windowsControlRefusal("pipe:policy")
	}
	return windowsPipeValidateDescriptor(sd)
}

// Microsoft Learn: GetNamedPipeClientProcessId / GetNamedPipeServerProcessId;
// OpenProcessToken; GetTokenInformation; GetProcessTimes. No credential bytes
// are exchanged until this retained-process check succeeds.
func windowsPipePeer(handle windows.Handle, server bool, expected *serviceControlEndpointRecord) error {
	query := windows.GetNamedPipeServerProcessId
	if server {
		query = windows.GetNamedPipeClientProcessId
	}
	var pid uint32
	if query(handle, &pid) != nil || pid == 0 {
		return windowsControlRefusal("peer:pid")
	}
	if expected != nil && uint64(pid) != uint64(expected.HostPID) {
		return windowsControlRefusal("peer:identity")
	}
	process, err := windowsOpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return windowsControlRefusal("peer:pid")
	}
	defer func() { _ = windows.CloseHandle(process) }()
	identity, alive, err := windowsProcessIdentity(process)
	if err != nil || !alive || (expected != nil && identity != expected.HostStartIdentity) {
		return windowsControlRefusal("peer:identity")
	}
	sid, err := windowsProcessTokenUser(process)
	owner, ownerErr := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || ownerErr != nil || sid == nil || owner == nil || owner.User.Sid == nil || !sid.Equals(owner.User.Sid) {
		return windowsControlRefusal("peer:sid")
	}
	var confirmed uint32
	if query(handle, &confirmed) != nil || confirmed != pid {
		return windowsControlRefusal("peer:pid")
	}
	_, alive, err = windowsProcessIdentity(process)
	if err != nil || !alive {
		return windowsControlRefusal("peer:identity")
	}
	return nil
}

type windowsPipeAddress struct{}

func (windowsPipeAddress) Network() string { return "pipe" }
func (windowsPipeAddress) String() string  { return "pipe" }

type windowsPipeDirection struct {
	serial   sync.Mutex
	deadline time.Time
	wake     windows.Handle
}
type windowsPipeConn struct {
	mu          sync.Mutex
	handle      windows.Handle
	server      bool
	closed      bool
	wrote       bool
	forceClose  bool
	disposed    bool
	closeOnce   sync.Once
	done        chan struct{}
	read, write windowsPipeDirection
}

func windowsNewPipeConn(handle windows.Handle, server bool) (*windowsPipeConn, error) {
	c := &windowsPipeConn{handle: handle, server: server, done: make(chan struct{})}
	var err error
	c.read.wake, err = windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, windowsControlRefusal("pipe:event")
	}
	c.write.wake, err = windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(c.read.wake)
		return nil, windowsControlRefusal("pipe:event")
	}
	return c, nil
}

func (c *windowsPipeConn) Read(p []byte) (int, error)  { return c.io(p, &c.read, false) }
func (c *windowsPipeConn) Write(p []byte) (int, error) { return c.io(p, &c.write, true) }

// Microsoft Learn: Synchronous and Overlapped Pipe I/O; CancelIoEx function
// (ioapiset.h); GetOverlappedResult function (ioapiset.h). The OVERLAPPED and
// caller buffer stay live until cancellation completion, including on Close.
func (c *windowsPipeConn) io(p []byte, direction *windowsPipeDirection, writing bool) (count int, result error) {
	defer runtime.KeepAlive(p)
	direction.serial.Lock()
	defer direction.serial.Unlock()
	defer func() {
		if writing && count > 0 {
			c.mu.Lock()
			c.wrote = true
			c.mu.Unlock()
		}
	}()
	if len(p) == 0 {
		return 0, nil
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, windowsControlRefusal("pipe:event")
	}
	defer func() { _ = windows.CloseHandle(event) }()
	operation := windows.Overlapped{HEvent: event}
	var transferred uint32
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	if !direction.deadline.IsZero() && !time.Now().Before(direction.deadline) {
		c.mu.Unlock()
		return 0, os.ErrDeadlineExceeded
	}
	_ = windows.ResetEvent(direction.wake)
	if writing {
		err = windows.WriteFile(c.handle, p, &transferred, &operation)
	} else {
		err = windowsPipeReadFile(c.handle, p, &transferred, &operation)
	}
	c.mu.Unlock()
	if err == nil {
		return int(transferred), nil
	}
	if !errors.Is(err, windows.ERROR_IO_PENDING) {
		return 0, windowsPipeIOError(err)
	}
	for {
		c.mu.Lock()
		closed, deadline := c.closed, direction.deadline
		_ = windows.ResetEvent(direction.wake)
		c.mu.Unlock()
		if closed {
			return c.cancelIO(&operation, net.ErrClosed)
		}
		wait := uint32(windows.INFINITE)
		if !deadline.IsZero() {
			left := time.Until(deadline)
			if left <= 0 {
				return c.cancelIO(&operation, os.ErrDeadlineExceeded)
			}
			millis := (left + time.Millisecond - 1) / time.Millisecond
			if millis < time.Duration(windows.INFINITE) {
				wait = uint32(millis)
			}
		}
		state, waitErr := windows.WaitForMultipleObjects([]windows.Handle{event, direction.wake}, false, wait)
		if waitErr != nil {
			return c.cancelIO(&operation, windowsControlRefusal("pipe:wait"))
		}
		if state == windows.WAIT_OBJECT_0 {
			err = windows.GetOverlappedResult(c.handle, &operation, &transferred, false)
			runtime.KeepAlive(p)
			return int(transferred), windowsPipeIOError(err)
		}
		if state != windows.WAIT_OBJECT_0+1 && state != uint32(windows.WAIT_TIMEOUT) {
			return c.cancelIO(&operation, windowsControlRefusal("pipe:wait"))
		}
	}
}

func (c *windowsPipeConn) cancelIO(operation *windows.Overlapped, reason error) (int, error) {
	_ = windows.CancelIoEx(c.handle, operation)
	var transferred uint32
	_ = windows.GetOverlappedResult(c.handle, operation, &transferred, true)
	return int(transferred), reason
}

// EOF is this transport's Go error-mapping policy for an ended pipe. Microsoft
// System Error Codes (0-499) describes 109 (ERROR_BROKEN_PIPE) as "The pipe has
// been ended" and 233 (ERROR_PIPE_NOT_CONNECTED) as "No process is on the other
// end of the pipe":
// https://learn.microsoft.com/en-us/windows/win32/debug/system-error-codes--0-499-
// Go 1.27.3 src/internal/poll/fd_windows.go, (*FD).Read, likewise maps
// ERROR_BROKEN_PIPE to io.EOF for kindPipe. That Go policy does not establish
// 233's behavior; the real disconnected-peer Windows tests witness both server
// DisconnectNamedPipe and handle close and log only their numeric error codes.
// Code 232 (ERROR_NO_DATA), "The pipe is being closed", remains a fixed pipe:io
// refusal: a closing transition alone is not an established end-of-stream.
func windowsPipeIOError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) {
		return io.EOF
	}
	if errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
		return net.ErrClosed
	}
	return windowsControlRefusal("pipe:io")
}

func (c *windowsPipeConn) Close() error {
	return c.closeWithDrain(true)
}

func (c *windowsPipeConn) closeWithDrain(drain bool) error {
	if !drain {
		c.mu.Lock()
		c.forceClose = true
		if !c.disposed {
			_ = windows.CancelIoEx(c.handle, nil)
		}
		c.mu.Unlock()
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		_ = windows.CancelIoEx(c.handle, nil)
		_ = windows.SetEvent(c.read.wake)
		_ = windows.SetEvent(c.write.wake)
		c.mu.Unlock()
		c.read.serial.Lock()
		c.write.serial.Lock()
		if c.server {
			c.drainPeer()
			_ = windows.DisconnectNamedPipe(c.handle)
		}
		c.mu.Lock()
		_ = windows.CloseHandle(c.handle)
		c.disposed = true
		c.mu.Unlock()
		_ = windows.CloseHandle(c.read.wake)
		_ = windows.CloseHandle(c.write.wake)
		c.write.serial.Unlock()
		c.read.serial.Unlock()
		close(c.done)
	})
	return nil
}

// DisconnectNamedPipe discards unread buffered output (Microsoft Learn:
// DisconnectNamedPipe function). After a response, permit the HTTP client to
// consume it and close first. This is a bounded overlapped EOF drain, never a
// blocking FlushFileBuffers call; listener shutdown bypasses the drain.
func (c *windowsPipeConn) drainPeer() {
	c.mu.Lock()
	if c.forceClose || !c.wrote {
		c.mu.Unlock()
		return
	}
	deadline := time.Now().Add(time.Second)
	if !c.write.deadline.IsZero() && c.write.deadline.Before(deadline) {
		deadline = c.write.deadline
	}
	c.mu.Unlock()
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return
	}
	defer func() { _ = windows.CloseHandle(event) }()
	buffer := make([]byte, 256)
	defer runtime.KeepAlive(buffer)
	for time.Now().Before(deadline) {
		operation := windows.Overlapped{HEvent: event}
		var transferred uint32
		c.mu.Lock()
		if c.forceClose {
			c.mu.Unlock()
			return
		}
		_ = windows.ResetEvent(event)
		err = windows.ReadFile(c.handle, buffer, &transferred, &operation)
		c.mu.Unlock()
		if errors.Is(err, windows.ERROR_IO_PENDING) {
			left := time.Until(deadline)
			if left <= 0 {
				_, _ = c.cancelIO(&operation, os.ErrDeadlineExceeded)
				return
			}
			state, waitErr := windows.WaitForSingleObject(event, uint32((left+time.Millisecond-1)/time.Millisecond))
			if waitErr != nil || state != windows.WAIT_OBJECT_0 {
				_, _ = c.cancelIO(&operation, os.ErrDeadlineExceeded)
				return
			}
			err = windows.GetOverlappedResult(c.handle, &operation, &transferred, false)
		}
		if err != nil {
			return
		}
	}
}
func (c *windowsPipeConn) LocalAddr() net.Addr                { return windowsPipeAddress{} }
func (c *windowsPipeConn) RemoteAddr() net.Addr               { return windowsPipeAddress{} }
func (c *windowsPipeConn) SetDeadline(t time.Time) error      { return c.setDeadline(t, true, true) }
func (c *windowsPipeConn) SetReadDeadline(t time.Time) error  { return c.setDeadline(t, true, false) }
func (c *windowsPipeConn) SetWriteDeadline(t time.Time) error { return c.setDeadline(t, false, true) }
func (c *windowsPipeConn) setDeadline(t time.Time, read, write bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	if read {
		c.read.deadline = t
		_ = windows.SetEvent(c.read.wake)
	}
	if write {
		c.write.deadline = t
		_ = windows.SetEvent(c.write.wake)
	}
	return nil
}

type windowsPipeListener struct {
	mu       sync.Mutex
	handles  []windows.Handle
	active   map[*windowsPipeConn]bool
	accepts  chan net.Conn
	failures chan error
	stop     chan struct{}
	closed   bool
	once     sync.Once
	workers  sync.WaitGroup
}

func windowsListenPipe(name string) (*windowsPipeListener, error) {
	descriptor, err := windowsPipeDescriptor()
	if err != nil {
		return nil, err
	}
	pointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, windowsControlRefusal("pipe:name")
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor, InheritHandle: 0}
	l := &windowsPipeListener{active: make(map[*windowsPipeConn]bool), accepts: make(chan net.Conn), failures: make(chan error, windowsPipeInstances), stop: make(chan struct{})}
	for index := 0; index < windowsPipeInstances; index++ {
		flags := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
		if index == 0 {
			flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
		}
		handle, createErr := windowsCreateNamedPipe(pointer, flags, windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, windowsPipeInstances, 64*1024, 64*1024, 0, &attributes)
		runtime.KeepAlive(descriptor)
		if createErr != nil {
			_ = l.Close()
			if index == 0 {
				return nil, errors.New("localcell: service_control_unavailable: pipe:first_instance")
			}
			return nil, windowsControlRefusal("pipe:create")
		}
		l.handles = append(l.handles, handle)
		if err := windowsPipeValidateHandle(handle); err != nil {
			_ = l.Close()
			return nil, err
		}
	}
	for _, handle := range l.handles {
		l.workers.Add(1)
		go l.acceptInstance(handle)
	}
	return l, nil
}

func (l *windowsPipeListener) acceptInstance(handle windows.Handle) {
	defer l.workers.Done()
	for {
		if err := l.connect(handle); err != nil {
			if !errors.Is(err, net.ErrClosed) {
				l.failures <- err
			}
			return
		}
		if err := windowsPipePeer(handle, true, nil); err != nil {
			step := "peer:identity"
			var refusal *serviceControlRefusal
			if errors.As(err, &refusal) && (refusal.step == "peer:pid" || refusal.step == "peer:identity" || refusal.step == "peer:sid") {
				step = refusal.step
			}
			serviceControlAcceptRejected(os.Stderr, step)
			_ = windows.DisconnectNamedPipe(handle)
			continue
		}
		var duplicate windows.Handle
		if windows.DuplicateHandle(windows.CurrentProcess(), handle, windows.CurrentProcess(), &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS) != nil {
			serviceControlAcceptRejected(os.Stderr, "duplicate")
			_ = windows.DisconnectNamedPipe(handle)
			continue
		}
		conn, err := windowsNewPipeConn(duplicate, true)
		if err != nil {
			serviceControlAcceptRejected(os.Stderr, "conn")
			_ = windows.CloseHandle(duplicate)
			_ = windows.DisconnectNamedPipe(handle)
			continue
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			_ = conn.Close()
			return
		}
		l.active[conn] = true
		l.mu.Unlock()
		select {
		case l.accepts <- conn:
		case <-l.stop:
			_ = conn.Close()
		}
		<-conn.done
		l.mu.Lock()
		delete(l.active, conn)
		closed := l.closed
		l.mu.Unlock()
		if closed {
			return
		}
	}
}

func (l *windowsPipeListener) connect(handle windows.Handle) error {
	for {
		event, err := windows.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			return windowsControlRefusal("pipe:event")
		}
		operation := windows.Overlapped{HEvent: event}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			_ = windows.CloseHandle(event)
			return net.ErrClosed
		}
		err = windows.ConnectNamedPipe(handle, &operation)
		l.mu.Unlock()
		if errors.Is(err, windows.ERROR_IO_PENDING) {
			var transferred uint32
			err = windows.GetOverlappedResult(handle, &operation, &transferred, true)
		}
		_ = windows.CloseHandle(event)
		if err == nil || errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			return nil
		}
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed {
			return net.ErrClosed
		}
		if errors.Is(err, windows.ERROR_NO_DATA) {
			_ = windows.DisconnectNamedPipe(handle)
			continue
		}
		return windowsControlRefusal("pipe:connect")
	}
}

func (l *windowsPipeListener) Accept() (net.Conn, error) {
	select {
	case <-l.stop:
		return nil, net.ErrClosed
	default:
	}
	select {
	case conn := <-l.accepts:
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed {
			_ = conn.Close()
			return nil, net.ErrClosed
		}
		return conn, nil
	case err := <-l.failures:
		return nil, err
	case <-l.stop:
		return nil, net.ErrClosed
	}
}
func (l *windowsPipeListener) Addr() net.Addr { return windowsPipeAddress{} }
func (l *windowsPipeListener) Close() error {
	l.once.Do(func() {
		l.mu.Lock()
		l.closed = true
		close(l.stop)
		active := make([]*windowsPipeConn, 0, len(l.active))
		for conn := range l.active {
			active = append(active, conn)
		}
		for _, handle := range l.handles {
			_ = windows.CancelIoEx(handle, nil)
		}
		l.mu.Unlock()
		for _, conn := range active {
			_ = conn.closeWithDrain(false)
		}
		l.workers.Wait()
		for _, handle := range l.handles {
			_ = windows.CloseHandle(handle)
		}
	})
	return nil
}

func windowsDialPipe(ctx context.Context, record serviceControlEndpointRecord) (net.Conn, error) {
	pointer, err := windows.UTF16PtrFromString(record.PipeName)
	if err != nil {
		return nil, windowsControlRefusal("endpoint:record")
	}
	// CreateFileW's SQOS limits impersonation to identification; nil attributes
	// make the opened handle non-inheritable. Never scan or try a derived name.
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		handle, openErr := windows.CreateFile(pointer, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if errors.Is(openErr, windows.ERROR_PIPE_BUSY) {
			timer := time.NewTimer(5 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if openErr != nil {
			return nil, windowsControlRefusal("pipe:open")
		}
		if err := windowsPipeValidateHandle(handle); err != nil {
			_ = windows.CloseHandle(handle)
			return nil, err
		}
		if err := windowsPipePeer(handle, false, &record); err != nil {
			_ = windows.CloseHandle(handle)
			return nil, err
		}
		conn, err := windowsNewPipeConn(handle, false)
		if err != nil {
			_ = windows.CloseHandle(handle)
			return nil, err
		}
		return conn, nil
	}
}
