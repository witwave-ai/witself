// This file is overlaid into localcell only by the command proof. It adapts
// net.Pipe to the production protocol without exporting a test API in the binary.
package localcell

import (
	"context"
	"encoding/json"
	"net"
	"time"
)

type CommandShutdownFixture struct {
	*shutdownProtocol
	host       net.Conn
	child      net.Conn
	request    serviceShutdownRequest
	handled    chan struct{}
	Ready      chan struct{}
	AllowClose chan struct{}
}

func NewCommandShutdownFixture(cancel context.CancelFunc, role string) (*CommandShutdownFixture, error) {
	pipe, err := newServiceControlPipeName("lc_command_fixture")
	if err != nil {
		return nil, err
	}
	nonce, err := shutdownRandom(16)
	if err != nil {
		return nil, err
	}
	auth, err := shutdownRandom(32)
	if err != nil {
		return nil, err
	}
	id, err := shutdownRandom(16)
	if err != nil {
		return nil, err
	}
	d := serviceShutdownDescriptor{Version: 1, InstallationID: "lc_command_fixture", Generation: "fixture", Component: role, BootID: "fixture-boot", PID: 200, StartIdentity: "fixture-start", PipeName: pipe + "-child-" + role, Nonce: nonce, Authentication: auth}
	if !validShutdownDescriptor(d) {
		return nil, shutdownRefused("shutdown:descriptor")
	}
	host, child := net.Pipe()
	f := &CommandShutdownFixture{shutdownProtocol: newShutdownProtocol(d, cancel), host: host, child: child, request: d.request("cancel", id), handled: make(chan struct{}), Ready: make(chan struct{}), AllowClose: make(chan struct{})}
	go func() {
		for {
			var request serviceShutdownRequest
			if shutdownReadFrame(child, &request) != nil {
				return
			}
			_ = f.handle(request, func(response serviceShutdownResponse) error { return json.NewEncoder(child).Encode(response) })
			f.handled <- struct{}{}
		}
	}()
	return f, nil
}

func (f *CommandShutdownFixture) RuntimeReady() error {
	if err := f.shutdownProtocol.RuntimeReady(); err != nil {
		return err
	}
	close(f.Ready)
	return nil
}

func (f *CommandShutdownFixture) Exchange(action string) (serviceShutdownResponse, error) {
	request := f.request
	request.Action = action
	if action == "release" {
		id, err := shutdownRandom(16)
		if err != nil {
			return serviceShutdownResponse{}, err
		}
		request.RequestID = id
	}
	if err := json.NewEncoder(f.host).Encode(request); err != nil {
		return serviceShutdownResponse{}, err
	}
	var response serviceShutdownResponse
	if err := shutdownReadFrame(f.host, &response); err != nil {
		return response, err
	}
	// An ACK is not a handler completion fence: cancellation runs after writing.
	<-f.handled
	return response, nil
}

func (f *CommandShutdownFixture) Deadline(deadline time.Time) {
	_ = f.host.SetDeadline(deadline)
	_ = f.child.SetDeadline(deadline)
}

func (f *CommandShutdownFixture) Close() error {
	<-f.AllowClose
	_ = f.host.Close()
	return f.child.Close()
}
