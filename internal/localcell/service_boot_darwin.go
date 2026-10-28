//go:build darwin

package localcell

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func serviceBootID() (string, error) {
	v, err := unix.SysctlTimeval("kern.boottime")
	if err != nil || v.Sec <= 0 {
		return "", errors.New("service_boot_identity_unavailable")
	}
	return fmt.Sprintf("darwin:%d:%d", v.Sec, v.Usec), nil
}
