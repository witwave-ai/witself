//go:build !darwin && !linux

package localcell

import (
	"errors"
	"testing"
)

func TestServiceForegroundControlCapabilityUnavailable(t *testing.T) {
	c, _ := unitCell(t)
	t.Setenv("WITSELF_RUNTIME", "local")
	if err := c.Run(t.Context(), SupervisorPolicy{}); !errors.Is(err, ErrServiceManagerUnavailable) {
		t.Fatal("unsupported foreground control capability was not recorded")
	}
	if _, err := c.Control(t.Context(), "status"); !errors.Is(err, ErrServiceManagerUnavailable) {
		t.Fatal("unsupported control capability was not recorded")
	}
	t.Log("NOT_RUN service_manager_unavailable: native control adapter is not implemented on this platform")
}
