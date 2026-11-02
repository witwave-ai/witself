package localcell

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestServiceUnsupportedAdapterCapability(t *testing.T) {
	adapter := unavailableServiceAdapter{}
	observation, err := adapter.Inspect(context.Background(), ServiceDescriptor{})
	if !errors.Is(err, ErrServiceManagerUnavailable) || observation.Available || observation.Fallback != "localcellharness run" {
		t.Fatal("unsupported adapter omitted capability")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		if _, err := NewServiceAdapter().Plan(context.Background(), ServiceDescriptor{}); !errors.Is(err, ErrServiceManagerUnavailable) {
			t.Fatal("unsupported platform selected native manager")
		}
	}
}
