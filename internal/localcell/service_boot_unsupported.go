//go:build !linux && !darwin && !windows

package localcell

func serviceBootID() (string, error) { return "", ErrServiceManagerUnavailable }
