//go:build !darwin && !linux && !windows

package localcell

import (
	"context"
	"net"
)

func serviceControlCapability() error { return ErrServiceManagerUnavailable }
func (c *Cell) validateServiceControlEndpoint(context.Context) error {
	return ErrServiceManagerUnavailable
}
func (c *Cell) listenServiceControl(context.Context) (net.Listener, error) {
	return nil, ErrServiceManagerUnavailable
}
func (c *Cell) removeServiceControlEndpoint(context.Context) error {
	return ErrServiceManagerUnavailable
}

func (c *Cell) dialServiceControl(context.Context) (net.Conn, error) {
	return nil, ErrServiceManagerUnavailable
}
