//go:build !windows

package localcell

import (
	"context"
	"net"
)

// Unix sockets do not publish a separate listener record.
type serviceControlHostState struct{}

func (c *Cell) listenRunServiceControl(ctx context.Context, _ ServiceDescriptor) (net.Listener, error) {
	return c.listenServiceControl(ctx)
}
