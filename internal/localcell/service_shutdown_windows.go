//go:build windows

package localcell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

const (
	shutdownLaunchPipe   = "private_pipe"
	shutdownLaunchLegacy = "legacy_console_foreground"
)

type windowsShutdownChannel struct {
	*shutdownProtocol
	listener *windowsPipeListener
	workers  sync.WaitGroup
}

type serviceShutdownReady struct {
	PID           int    `json:"pid"`
	StartIdentity string `json:"start_identity"`
	BootID        string `json:"boot_id"`
}

// Synthetic fault injection. Production always writes the authenticated ACK.
var windowsShutdownReply = func(_ serviceShutdownRequest, _ serviceShutdownResponse) bool { return true }

// Native mutation witness; no callback is installed in production.
var windowsShutdownBeforeForce func(*ProcessRecord)

// Native proof fence runs after any deferred cancellation callback completes.
var windowsShutdownHandlerCompleted func(serviceShutdownRequest)

// InstallLocalShutdown executes no filesystem operation and starts no worker
// when the optional local shutdown configuration is absent.
func InstallLocalShutdown(_ context.Context, cancel context.CancelFunc, cfg runtimeconfig.LocalShutdown, component string) (LocalShutdownChannel, error) {
	if cfg == (runtimeconfig.LocalShutdown{}) {
		return nil, nil
	}
	if cfg.Transport != runtimeconfig.LocalShutdownNamedPipe || cancel == nil || (component != "server" && component != "worker") {
		return nil, shutdownRefused("shutdown:config")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(cfg.DescriptorPath)))
	d, err := readShutdownDescriptor(root, cfg.DescriptorPath)
	if err != nil {
		return nil, err
	}
	if d.Component != component || d.PID != os.Getpid() {
		return nil, shutdownRefused("shutdown:incarnation")
	}
	self, err := windowsOpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(os.Getpid()))
	if err != nil {
		return nil, shutdownRefused("shutdown:incarnation")
	}
	identity, alive, identityErr := windowsProcessIdentity(self)
	_ = windows.CloseHandle(self)
	boot, bootErr := serviceBootID()
	if identityErr != nil || !alive || identity != d.StartIdentity || bootErr != nil || boot != d.BootID {
		return nil, shutdownRefused("shutdown:descriptor")
	}
	c, err := New(Config{Root: root})
	if err != nil {
		return nil, shutdownRefused("shutdown:descriptor")
	}
	service, err := c.ServiceDescriptor()
	if err != nil || service.InstallationID != d.InstallationID || service.Payload.Generation != d.Generation {
		return nil, shutdownRefused("shutdown:descriptor")
	}
	listener, err := windowsListenPipe(d.PipeName)
	if err != nil {
		return nil, shutdownRefused("shutdown:pipe")
	}
	channel := &windowsShutdownChannel{shutdownProtocol: newShutdownProtocol(d, cancel), listener: listener}
	channel.workers.Add(1)
	go channel.serve()
	marker := serviceShutdownReady{d.PID, d.StartIdentity, d.BootID}
	if writeAtomic(cfg.DescriptorPath+".ready", marker, nil) != nil {
		_ = channel.Close()
		return nil, shutdownRefused("shutdown:descriptor")
	}
	return channel, nil
}

func readShutdownDescriptor(root, path string) (serviceShutdownDescriptor, error) {
	if !shutdownDescriptorPath(root, path) || validatePrivateTree(root) != nil || privateDir(root, false) != nil ||
		privateDir(filepath.Join(root, "service"), false) != nil || privateDir(filepath.Join(root, "service", "children"), false) != nil || servicePrivateRecord(path) != nil {
		return serviceShutdownDescriptor{}, shutdownRefused("shutdown:descriptor")
	}
	f, err := openPrivateFile(path, os.O_RDONLY, 0)
	if err != nil {
		return serviceShutdownDescriptor{}, shutdownRefused("shutdown:descriptor")
	}
	defer func() { _ = f.Close() }()
	return decodeShutdownDescriptor(f)
}

func (c *windowsShutdownChannel) Close() error {
	err := c.listener.Close()
	c.workers.Wait()
	return err
}

func (c *windowsShutdownChannel) serve() {
	defer c.workers.Done()
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			return
		}
		c.workers.Add(1)
		go func() {
			defer c.workers.Done()
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			var request serviceShutdownRequest
			if shutdownReadFrame(conn, &request) != nil {
				return
			}
			pipe, ok := conn.(*windowsPipeConn)
			if !ok || windowsPipePeer(pipe.handle, true, nil) != nil {
				return
			}
			_ = c.handle(request, func(response serviceShutdownResponse) error {
				if !windowsShutdownReply(request, response) {
					return nil
				}
				return json.NewEncoder(conn).Encode(response)
			})
			if windowsShutdownHandlerCompleted != nil {
				windowsShutdownHandlerCompleted(request)
			}
		}()
	}
}

func windowsShutdownRequest(ctx context.Context, d serviceShutdownDescriptor, request serviceShutdownRequest) (serviceShutdownResponse, error) {
	endpoint := serviceControlEndpointRecord{PipeName: d.PipeName, HostPID: d.PID, HostStartIdentity: d.StartIdentity}
	conn, err := windowsDialPipe(ctx, endpoint)
	if err != nil {
		return serviceShutdownResponse{}, shutdownRefused("shutdown:pipe")
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(250 * time.Millisecond)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	_ = conn.SetDeadline(deadline)
	if json.NewEncoder(conn).Encode(request) != nil {
		return serviceShutdownResponse{}, errShutdownAckLost
	}
	var response serviceShutdownResponse
	if shutdownReadFrame(conn, &response) != nil {
		return response, errShutdownAckLost
	}
	if !response.Accepted || response.RequestID != request.RequestID || response.Nonce != request.Nonce {
		return serviceShutdownResponse{}, shutdownRefused("shutdown:ack")
	}
	return response, nil
}

type retainedShutdownProcess struct {
	handle    windows.Handle
	terminate bool
}

var shutdownProcesses = struct {
	sync.Mutex
	entries map[string]retainedShutdownProcess
}{entries: make(map[string]retainedShutdownProcess)}

func shutdownProcessKey(record ProcessRecord) string {
	return record.Root + "\x00" + record.Component + "\x00" + fmt.Sprint(record.PID) + "\x00" + record.StartTime
}

func retainShutdownProcess(record ProcessRecord, handle windows.Handle) error {
	if alive, err := shutdownVerifyProcess(handle, record); err != nil || !alive {
		return shutdownRefused("shutdown:incarnation")
	}
	var retained windows.Handle
	if windows.DuplicateHandle(windows.CurrentProcess(), handle, windows.CurrentProcess(), &retained, 0, false, windows.DUPLICATE_SAME_ACCESS) != nil {
		return shutdownRefused("shutdown:force_rights")
	}
	key := shutdownProcessKey(record)
	shutdownProcesses.Lock()
	shutdownProcesses.entries[key] = retainedShutdownProcess{retained, true}
	shutdownProcesses.Unlock()
	go func() {
		_, _ = windows.WaitForSingleObject(retained, windows.INFINITE)
		shutdownProcesses.Lock()
		delete(shutdownProcesses.entries, key)
		_ = windows.CloseHandle(retained)
		shutdownProcesses.Unlock()
	}()
	return nil
}

// A restarted host can request ordinary cancellation after verifying identity,
// but cannot reconstruct launch-time escalation rights by reopening a PID.
func openShutdownProcess(record ProcessRecord) (windows.Handle, bool, bool, error) {
	shutdownProcesses.Lock()
	entry, ok := shutdownProcesses.entries[shutdownProcessKey(record)]
	if ok {
		var duplicate windows.Handle
		err := windows.DuplicateHandle(windows.CurrentProcess(), entry.handle, windows.CurrentProcess(), &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS)
		shutdownProcesses.Unlock()
		if err != nil {
			return 0, false, false, shutdownRefused("shutdown:incarnation")
		}
		return duplicate, true, entry.terminate, nil
	}
	shutdownProcesses.Unlock()
	handle, err := windowsOpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(record.PID))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, shutdownRefused("shutdown:incarnation")
	}
	return handle, true, false, nil
}

func shutdownVerifyProcess(handle windows.Handle, record ProcessRecord) (bool, error) {
	identity, alive, err := windowsProcessIdentity(handle)
	if err != nil || (alive && identity != record.StartTime) {
		return false, shutdownRefused("shutdown:incarnation")
	}
	if !alive {
		// A signaled handle still has a creation identity. Verify it before
		// removing a record; a dead PID-reused object is not our incarnation.
		var created, exited, kernel, user windows.Filetime
		if windows.GetProcessTimes(handle, &created, &exited, &kernel, &user) != nil || fmt.Sprintf("windows:%d:%d", created.HighDateTime, created.LowDateTime) != record.StartTime {
			return false, shutdownRefused("shutdown:incarnation")
		}
		return false, nil
	}
	owner, err := windows.GetCurrentProcessToken().GetTokenUser()
	sid, sidErr := windowsProcessTokenUser(handle)
	if err != nil || sidErr != nil || owner == nil || owner.User.Sid == nil || sid == nil || !sid.Equals(owner.User.Sid) {
		return false, shutdownRefused("shutdown:incarnation")
	}
	return true, nil
}

func (c *Cell) stopChildOutcome(ctx context.Context, component string, state ProcessState) serviceStopOutcome {
	return c.stopChildOutcomeWithBudget(ctx, component, state, serviceAppShutdownTimeout)
}

func (c *Cell) stopChildOutcomeWithBudget(ctx context.Context, component string, state ProcessState, appBudget time.Duration) (outcome serviceStopOutcome) {
	result := func(kind serviceStopKind, detail string) serviceStopOutcome {
		return serviceStopOutcome{Kind: kind, Component: component, Detail: detail}
	}
	if ctx.Err() != nil {
		return result(serviceStopUnknown, "shutdown:deadline")
	}
	record, err := c.windowsStopRecord(component, state)
	if err != nil {
		return result(serviceStopRefused, "shutdown:incarnation")
	}
	defer func() {
		if !outcome.permitsDrain() {
			return
		}
		var current ProcessRecord
		if readJSON(c.path("run", component+".json"), &current) != nil || current != record || removePrivateFile(c.path("run", component+".json")) != nil {
			outcome = result(serviceStopRefused, "shutdown:incarnation")
		}
	}()
	if record.LaunchKind != shutdownLaunchPipe {
		if record.LaunchKind != shutdownLaunchLegacy {
			return result(serviceStopRefused, "shutdown:pipe")
		}
		if record.LaunchPending {
			if terminateWindowsPending(ctx, record) != nil {
				return result(serviceStopUnknown, "shutdown:deadline")
			}
			return result(serviceStopAlreadyExited, "")
		}
		if c.terminateLegacyChild(ctx, component, state) != nil {
			return result(serviceStopRefused, "shutdown:pipe")
		}
		handle, alive, openErr := openVerifiedWindowsProcess(record, 0)
		if openErr != nil {
			return result(serviceStopUnknown, "shutdown:incarnation")
		}
		if !alive {
			return result(serviceStopAlreadyExited, "")
		}
		defer func() { _ = windows.CloseHandle(handle) }()
		if waitShutdownHandle(ctx, handle, appBudget) {
			return result(serviceStopGraceful, "")
		}
		return result(serviceStopUnknown, "shutdown:deadline")
	}
	d, err := readShutdownDescriptor(c.cfg.Root, record.ShutdownDescriptor)
	if err != nil || !shutdownDescriptorMatches(d, record) {
		return result(serviceStopRefused, "shutdown:descriptor")
	}
	service, err := c.ServiceDescriptor()
	boot, bootErr := serviceBootID()
	if err != nil || bootErr != nil || d.BootID != boot || d.InstallationID != service.InstallationID || d.Generation != service.Payload.Generation {
		return result(serviceStopRefused, "shutdown:descriptor")
	}
	handle, alive, forceRights, err := openShutdownProcess(record)
	if err != nil {
		return result(serviceStopRefused, "shutdown:incarnation")
	}
	if !alive {
		return result(serviceStopAlreadyExited, "")
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if alive, err = shutdownVerifyProcess(handle, record); err != nil {
		return result(serviceStopRefused, "shutdown:incarnation")
	}
	if !alive {
		return result(serviceStopAlreadyExited, "")
	}
	id, err := shutdownRandom(16)
	if err != nil {
		return result(serviceStopRefused, "shutdown:ack")
	}
	request := d.request("cancel", id)
	return runServiceShutdownPolicy(ctx, record, request, forceRights, appBudget, serviceShutdownProcess{
		verify: func(candidate ProcessRecord) (bool, error) { return shutdownVerifyProcess(handle, candidate) },
		send: func(ctx context.Context, request serviceShutdownRequest) (serviceShutdownResponse, error) {
			return windowsShutdownRequest(ctx, d, request)
		},
		wait:        func(ctx context.Context, budget time.Duration) bool { return waitShutdownHandle(ctx, handle, budget) },
		force:       func() error { return windows.TerminateProcess(handle, 125) },
		beforeForce: windowsShutdownBeforeForce,
	})
}

func waitShutdownHandle(ctx context.Context, handle windows.Handle, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	for {
		wait := uint32(max(0, min(25, time.Until(deadline).Milliseconds())))
		state, err := windows.WaitForSingleObject(handle, wait)
		if err != nil {
			return false
		}
		if state == windows.WAIT_OBJECT_0 {
			return true
		}
		if state != uint32(windows.WAIT_TIMEOUT) || ctx.Err() != nil || !time.Now().Before(deadline) {
			return false
		}
	}
}

func (c *Cell) prepareShutdownLaunch(component string) (serviceShutdownDescriptor, string, error) {
	if component == "postgres" || os.Getenv("WITSELF_RUNTIME") != "local" {
		return serviceShutdownDescriptor{}, "", nil
	}
	service, err := c.ServiceDescriptor()
	if errors.Is(err, os.ErrNotExist) {
		return serviceShutdownDescriptor{}, "", nil
	}
	if err != nil {
		return serviceShutdownDescriptor{}, "", shutdownRefused("shutdown:descriptor")
	}
	boot, err := serviceBootID()
	if err != nil {
		return serviceShutdownDescriptor{}, "", shutdownRefused("shutdown:descriptor")
	}
	name, err := newServiceControlPipeName(service.InstallationID)
	if err != nil {
		return serviceShutdownDescriptor{}, "", shutdownRefused("shutdown:descriptor")
	}
	nonce, err := shutdownRandom(16)
	if err != nil {
		return serviceShutdownDescriptor{}, "", err
	}
	auth, err := shutdownRandom(32)
	if err != nil {
		return serviceShutdownDescriptor{}, "", err
	}
	if privateDir(c.path("service", "children"), true) != nil {
		return serviceShutdownDescriptor{}, "", shutdownRefused("shutdown:descriptor")
	}
	path := c.path("service", "children", component+"-"+strings.TrimPrefix(name, serviceControlPipePrefix+service.InstallationID+"-")+".json")
	d := serviceShutdownDescriptor{Version: 1, InstallationID: service.InstallationID, Generation: service.Payload.Generation, Component: component, BootID: boot, PipeName: name + "-child-" + component, Nonce: nonce, Authentication: auth}
	return d, path, nil
}

func waitShutdownReady(ctx context.Context, path string, d serviceShutdownDescriptor, handle windows.Handle) error {
	for {
		var ready serviceShutdownReady
		if servicePrivateRecord(path+".ready") == nil && readJSON(path+".ready", &ready) == nil {
			if ready.PID != d.PID || ready.StartIdentity != d.StartIdentity || ready.BootID != d.BootID {
				return shutdownRefused("shutdown:descriptor")
			}
			return nil
		}
		_, alive, err := windowsProcessIdentity(handle)
		if err != nil || !alive {
			return shutdownRefused("shutdown:incarnation")
		}
		select {
		case <-ctx.Done():
			return shutdownRefused("shutdown:deadline")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
