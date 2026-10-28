//go:build !linux && !darwin

package localcell

func serviceBootID() (string, error) { return "", ErrServiceManagerUnavailable }
