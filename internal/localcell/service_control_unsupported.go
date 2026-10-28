//go:build !darwin && !linux

package localcell

import "net"

func serviceControlCapability() error                       { return ErrServiceManagerUnavailable }
func (c *Cell) validateServiceSocket() error                { return ErrServiceManagerUnavailable }
func (c *Cell) listenServiceControl() (net.Listener, error) { return nil, ErrServiceManagerUnavailable }
func (c *Cell) removeServiceSocket() error                  { return ErrServiceManagerUnavailable }
