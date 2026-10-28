//go:build darwin || linux

package localcell

import (
	"errors"
	"net"
	"os"
	"syscall"
)

func serviceControlCapability() error { return nil }

func (c *Cell) validateServiceSocket() error {
	info, err := os.Lstat(c.path("service", "control.sock"))
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		return ErrServiceControlRefused
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return ErrServiceControlRefused
	}
	return nil
}

func (c *Cell) listenServiceControl() (net.Listener, error) {
	path := c.path("service", "control.sock")
	if _, err := os.Lstat(path); err == nil {
		if err := c.validateServiceSocket(); err != nil {
			return nil, err
		}
		if err := removePrivateFile(path); err != nil {
			return nil, ErrServiceControlRefused
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, ErrServiceControlRefused
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.New("localcell: service_control_unavailable")
	}
	// The enclosing 0700 directory protects the socket before chmod completes.
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = removePrivateFile(path)
		return nil, ErrServiceControlRefused
	}
	return listener, nil
}

func (c *Cell) removeServiceSocket() error {
	if err := c.validateServiceSocket(); err != nil {
		return err
	}
	return removePrivateFile(c.path("service", "control.sock"))
}
