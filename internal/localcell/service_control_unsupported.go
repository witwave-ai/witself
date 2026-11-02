//go:build !darwin && !linux && !windows

package localcell

import (
	"context"
	"net"
)

func serviceControlCapability() error                       { return ErrServiceManagerUnavailable }
func (c *Cell) validateServiceControlEndpoint() error       { return ErrServiceManagerUnavailable }
func (c *Cell) listenServiceControl() (net.Listener, error) { return nil, ErrServiceManagerUnavailable }
func (c *Cell) removeServiceControlEndpoint() error         { return ErrServiceManagerUnavailable }

func (c *Cell) dialServiceControl(context.Context) (net.Conn, error) {
	return nil, ErrServiceManagerUnavailable
}
