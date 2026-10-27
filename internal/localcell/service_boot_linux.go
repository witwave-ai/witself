//go:build linux

package localcell

import (
	"errors"
	"os"
	"strings"
)

func serviceBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return "", errors.New("service_boot_identity_unavailable")
	}
	return "linux:" + strings.TrimSpace(string(b)), nil
}
