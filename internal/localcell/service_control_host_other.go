//go:build !windows

package localcell

import "net"

// Unix sockets do not publish a separate listener record.
type serviceControlHostState struct{}

func (c *Cell) listenRunServiceControl(_ ServiceDescriptor) (net.Listener, error) {
	return c.listenServiceControl()
}
