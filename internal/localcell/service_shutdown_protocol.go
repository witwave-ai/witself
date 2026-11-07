package localcell

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
)

// shutdownProtocol owns authentication, release and idempotency independently
// of the platform transport. The Windows endpoint and command proofs use it.
type shutdownProtocol struct {
	descriptor serviceShutdownDescriptor
	cancel     context.CancelFunc
	released   chan struct{}
	mu         sync.Mutex
	phase      string
	canceled   bool
	cancelOnce sync.Once
	replies    map[string]serviceShutdownResponse
	actions    map[string]string
}

func newShutdownProtocol(d serviceShutdownDescriptor, cancel context.CancelFunc) *shutdownProtocol {
	return &shutdownProtocol{descriptor: d, cancel: cancel, released: make(chan struct{}), phase: shutdownPhaseBarrier, replies: make(map[string]serviceShutdownResponse), actions: make(map[string]string)}
}

// handle completes only after the ACK attempt and cancellation callback. An
// unsuccessful ACK write still requests cancellation; retries reuse the ACK.
func (c *shutdownProtocol) handle(request serviceShutdownRequest, write func(serviceShutdownResponse) error) error {
	response := c.accept(request)
	if response.Accepted && request.Action == "cancel" {
		defer c.cancelOnce.Do(c.cancel)
	}
	return write(response)
}

func (c *shutdownProtocol) WaitRelease(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.released:
		return ctx.Err()
	}
}

func (c *shutdownProtocol) RuntimeReady() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.phase != shutdownPhaseStarting || c.canceled {
		return shutdownRefused("shutdown:migration")
	}
	c.phase = shutdownPhaseReady
	return nil
}

func (c *shutdownProtocol) accept(request serviceShutdownRequest) serviceShutdownResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	refused := serviceShutdownResponse{}
	if !c.descriptor.accepts(request) {
		return refused
	}
	if previous, ok := c.replies[request.RequestID]; ok {
		if c.actions[request.RequestID] != request.Action {
			return refused
		}
		return previous
	}
	if len(c.replies) >= 32 {
		return refused
	}
	if request.Action == "release" {
		if c.canceled {
			return refused
		}
		if c.phase == shutdownPhaseBarrier {
			c.phase = shutdownPhaseStarting
			close(c.released)
		}
	} else if !c.canceled {
		c.canceled = true
	}
	response := serviceShutdownResponse{RequestID: request.RequestID, Nonce: request.Nonce, Accepted: true, Phase: c.phase}
	c.replies[request.RequestID] = response
	c.actions[request.RequestID] = request.Action
	return response
}

func shutdownReadFrame(r io.Reader, v any) error {
	frame, err := bufio.NewReader(io.LimitReader(r, 4097)).ReadBytes('\n')
	if err != nil || len(frame) > 4096 {
		return shutdownRefused("shutdown:ack")
	}
	decoder := json.NewDecoder(bytes.NewReader(frame))
	decoder.DisallowUnknownFields()
	if decoder.Decode(v) != nil || decoder.Decode(new(any)) != io.EOF {
		return shutdownRefused("shutdown:ack")
	}
	return nil
}
